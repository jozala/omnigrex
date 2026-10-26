//go:build integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/workspace"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	mobyclient "github.com/moby/moby/client"
)

const workspaceFixtureArchiveLimit = 16 << 20

// The foreground process imitates a request waiting for its MCP response.
// Its independent background writer must freeze for the entire seal callback.
const workspaceFixtureCommand = `
set -eu
(
  i=0
  while :; do
    i=$((i+1))
    printf '%s\n' "$i" > /workspace/racing.next
    mv /workspace/racing.next /workspace/racing.txt
    sleep 0.01
  done
) &
printf 'outstanding\n' > /workspace/request.txt
while [ ! -f /workspace/response ]; do sleep 0.01; done
printf 'delivered\n' > /workspace/request.next
mv /workspace/request.next /workspace/request.txt
wait
`

func TestWorkspaceSourceDockerQuiescesMidRequest(t *testing.T) {
	fixture := newWorkspaceDockerFixture(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture.waitCounter(t, ctx, 0)
	source, err := NewWorkspaceSource()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	request := fixture.captureRequest(execution.PublicationCapture)
	var captured map[string]workspaceFixtureEntry
	started := time.Now()
	err = source.ReadWorkspace(ctx, request, func(reader io.Reader) error {
		var err error
		captured, err = readWorkspaceFixtureArchive(reader, "workspace/")
		if err != nil {
			return err
		}
		// Docker serializes inspect against an in-progress archive transfer.
		// Drain the stream before making independent SDK observations.
		fixture.assertState(t, ctx, true, true)
		before := fixture.readFile(t, ctx, "/workspace/racing.txt")
		// Holding the callback open proves quiescence covers sealing, not merely
		// acquisition of the archive stream. The writer runs every 10ms.
		time.Sleep(150 * time.Millisecond)
		fixture.assertState(t, ctx, true, true)
		after := fixture.readFile(t, ctx, "/workspace/racing.txt")
		if before != after || string(captured["racing.txt"].content) != before {
			t.Errorf("writer changed while sealing: before=%q archive=%q after=%q", before, captured["racing.txt"].content, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("mid-request transfer took %s", elapsed)
	}
	assertWorkspaceFixtureInventory(t, captured, fixture.head)
	if string(captured["request.txt"].content) != "outstanding\n" {
		t.Fatal("capture waited for the outstanding request to complete")
	}
	fixture.assertState(t, ctx, true, false)
	counter, err := strconv.Atoi(strings.TrimSpace(string(captured["racing.txt"].content)))
	if err != nil {
		t.Fatal(err)
	}
	fixture.waitCounter(t, ctx, counter)
	// Deliver the imitated response only after capture. Both foreground and
	// background processes remain usable after the pause/unpause cycle.
	fixture.copyFile(t, ctx, "/workspace", "response", []byte("ok\n"))
	fixture.waitFile(t, ctx, "/workspace/request.txt", "delivered\n")

	t.Run("seal failure resumes writers", func(t *testing.T) {
		failure := errors.New("sealing failed")
		before, err := strconv.Atoi(strings.TrimSpace(fixture.readFile(t, ctx, "/workspace/racing.txt")))
		if err != nil {
			t.Fatal(err)
		}
		err = source.ReadWorkspace(ctx, request, func(reader io.Reader) error {
			// Fail before draining the HTTP body, exercising archive close before
			// resume while Docker still holds its per-container archive lock.
			if _, err := io.CopyN(io.Discard, reader, 1024); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatalf("seal failure = %v", err)
		}
		fixture.assertState(t, ctx, true, false)
		fixture.waitCounter(t, ctx, before)
	})
	t.Run("cancellation resumes writers", func(t *testing.T) {
		before, err := strconv.Atoi(strings.TrimSpace(fixture.readFile(t, ctx, "/workspace/racing.txt")))
		if err != nil {
			t.Fatal(err)
		}
		captureCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		err = source.ReadWorkspace(captureCtx, request, func(reader io.Reader) error {
			if _, err := io.CopyN(io.Discard, reader, 1024); err != nil {
				return err
			}
			cancel()
			return captureCtx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled seal = %v", err)
		}
		fixture.assertState(t, ctx, true, false)
		fixture.waitCounter(t, ctx, before)
	})
}

func TestWorkspaceSourceDockerTerminalCaptureSurvivesRemovalAndRestart(t *testing.T) {
	fixture := newWorkspaceDockerFixture(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture.waitCounter(t, ctx, 0)
	source, err := NewWorkspaceSource()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	root := t.TempDir()
	options := workspace.Options{WorkspaceRoot: filepath.Join(root, "unavailable-live-workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"), CaptureRoot: filepath.Join(root, "captures"), CaptureSource: source}
	lifecycle, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	terminalRequest := fixture.captureRequest(execution.TerminalCapture)
	if ref, err := lifecycle.Capture(ctx, terminalRequest); !errors.Is(err, execution.ErrUnknownEffect) || ref.SHA256 != "" {
		t.Fatalf("running terminal capture = %#v, %v", ref, err)
	}
	fixture.assertState(t, ctx, true, false)
	seedHead := fixture.head
	fixture.copyFile(t, ctx, "/workspace", "feature.txt", []byte("guest-only committed change\n"))
	fixture.git(t, ctx, "add", "feature.txt")
	fixture.git(t, ctx, "-c", "user.name=Workspace Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "guest change")
	fixture.head = strings.TrimSpace(fixture.readFile(t, ctx, "/workspace/.git/refs/heads/main"))
	if fixture.head == seedHead {
		t.Fatal("guest change did not produce a new commit")
	}
	// The approved remote still contains only the original trusted seed.
	// Publication must import the new commit from the sealed capture.
	if output, err := exec.CommandContext(ctx, "git", "--git-dir", fixture.remote, "cat-file", "-e", fixture.head+"^{commit}").CombinedOutput(); err == nil {
		t.Fatalf("guest-only commit already exists in the trusted seed remote: %s", output)
	}
	publication, err := lifecycle.Capture(ctx, fixture.captureRequest(execution.PublicationCapture))
	if err != nil {
		t.Fatal(err)
	}
	publicationEntries := openWorkspaceFixtureCapture(t, ctx, lifecycle, publication)
	paths, err := lifecycle.Paths(fixture.key.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	// Build the trusted publication solely from transferred Git objects. This
	// also proves the archive carries a complete usable repository, not just
	// selected metadata files that happen to satisfy inventory assertions.
	materializeWorkspaceFixturePublication(t, ctx, paths.Publication, publicationEntries, fixture.head)
	const controllerCredential = "controller-only-fixture-credential"
	publishedHead := strings.TrimSpace(string(publicationEntries[".git/refs/heads/main"].content))
	if publishedHead != fixture.head {
		t.Fatalf("captured publication tip = %q, want %q", publishedHead, fixture.head)
	}
	push := exec.CommandContext(ctx, "git", "push", fixture.remote, publishedHead+":refs/heads/published")
	push.Dir = paths.Publication
	push.Env = append(os.Environ(), "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=http.extraHeader", "GIT_CONFIG_VALUE_1=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+controllerCredential)))
	if output, err := push.CombinedOutput(); err != nil {
		t.Fatalf("trusted controller publication: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(workspaceFixtureGit(t, ctx, fixture.remote, "rev-parse", "refs/heads/published")); got != publishedHead {
		t.Fatalf("trusted remote published tip = %q, want %q", got, publishedHead)
	}
	observation := workspace.PublicationObservation{RepositoryURL: fixture.remote, Credential: controllerCredential, Revision: publishedHead}
	if equal, err := lifecycle.CaptureTreesEqual(ctx, publication, observation); err != nil || !equal {
		t.Fatalf("trusted inspection of separately materialized capture = %t, %v", equal, err)
	}
	seedObservation := observation
	seedObservation.Revision = seedHead
	if equal, err := lifecycle.CaptureTreesEqual(ctx, publication, seedObservation); err != nil || equal {
		t.Fatalf("trusted comparison ignored the pinned revision and used the matching cache = %t, %v", equal, err)
	}
	// Comparison must freshly clone the independently authorized pinned remote.
	// Remove the controller's import/publication cache before further checks.
	if err := os.RemoveAll(options.PublicationRoot); err != nil {
		t.Fatal(err)
	}
	if equal, err := lifecycle.CaptureTreesEqual(ctx, publication, observation); err != nil || !equal {
		t.Fatalf("trusted inspection without publication cache = %t, %v", equal, err)
	}
	if strings.Contains(string(publicationEntries[".git/config"].content), controllerCredential) {
		t.Fatal("controller credential entered the guest repository")
	}
	counter, err := strconv.Atoi(strings.TrimSpace(string(publicationEntries["racing.txt"].content)))
	if err != nil {
		t.Fatal(err)
	}
	fixture.waitCounter(t, ctx, counter)
	fixture.copyFile(t, ctx, "/workspace", "late-edit.txt", []byte("changed after publication\n"))

	// Simulate a lost main-creation acknowledgement: the resource exists, but
	// its immutable ID has not yet been bound in the execution ledger.
	ledger := newExecutionTestLedger()
	fixture.reserveMain(t, ctx, ledger)
	cleaner, err := NewExactLabelCleaner(ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
	if err != nil {
		t.Fatal(err)
	}
	defer cleaner.Close()
	for range 2 {
		if err := cleaner.EnsureKeyStopped(ctx, fixture.key); err != nil {
			t.Fatal(err)
		}
		fixture.assertState(t, ctx, false, false)
	}
	reservation := ledger.records[fixture.key]
	if !reservation.Revoked || !reservation.CreationSettled || reservation.Resource != execution.Ref(fixture.id) {
		t.Fatalf("stop did not resolve/revoke lost creation: %#v", reservation)
	}
	if err := ledger.BeginExecutionPhase(ctx, fixture.key, execution.Main); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("stopped epoch admitted creation: %v", err)
	}
	terminal, err := lifecycle.Capture(ctx, terminalRequest)
	if err != nil {
		t.Fatal(err)
	}
	terminalEntries := openWorkspaceFixtureCapture(t, ctx, lifecycle, terminal)
	assertWorkspaceFixtureInventory(t, terminalEntries, fixture.head)
	if string(terminalEntries["late-edit.txt"].content) != "changed after publication\n" || terminal.SHA256 == publication.SHA256 {
		t.Fatal("terminal capture reused publication-time evidence")
	}
	fixture.assertState(t, ctx, false, false)
	if err := cleaner.EnsureKeyAbsent(ctx, fixture.key); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.client.ContainerInspect(ctx, fixture.id, mobyclient.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("resource remains after removal: %v", err)
	}
	if _, err := fixture.client.VolumeRemove(ctx, fixture.volume, mobyclient.VolumeRemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	// Reopen with no source and no live checkout or Docker volume. Retained
	// evidence alone must support deferred corroboration and operation retries.
	options.CaptureSource = nil
	restarted, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := restarted.TerminalCapture(ctx, fixture.key)
	if err != nil || retained != terminal {
		t.Fatalf("retained terminal capture = %#v, %v", retained, err)
	}
	if retried, err := restarted.Capture(ctx, terminalRequest); err != nil || retried != terminal {
		t.Fatalf("post-removal capture retry = %#v, %v", retried, err)
	}
	reopened := openWorkspaceFixtureCapture(t, ctx, restarted, retained)
	if !maps.EqualFunc(reopened, terminalEntries, func(a, b workspaceFixtureEntry) bool {
		return a.kind == b.kind && a.mode == b.mode && a.link == b.link && bytes.Equal(a.content, b.content)
	}) {
		t.Fatal("capture inventory changed after resource removal and restart")
	}
	if equal, err := restarted.CaptureTreesEqual(ctx, retained, observation); err != nil || equal {
		t.Fatalf("retained terminal capture hid late dirty changes = %t, %v", equal, err)
	}
	if equal, err := restarted.CaptureTreesEqual(ctx, publication, observation); err != nil || !equal {
		t.Fatalf("retained publication inspection after restart = %t, %v", equal, err)
	}
	if _, err := os.Stat(options.WorkspaceRoot); !os.IsNotExist(err) {
		t.Fatalf("capture consulted or materialized a live host workspace: %v", err)
	}
}

func TestWorkspaceSourceDockerRejectsOwnershipMismatch(t *testing.T) {
	for _, label := range []string{RuntimeProcessMarkerLabel, RuntimeProcessAssignmentLabel, RuntimeProcessSessionLabel, RuntimeProcessTurnLabel, RuntimeProcessEpochLabel, RuntimeProfileIdentityLabel} {
		t.Run(label, func(t *testing.T) {
			fixture := newWorkspaceDockerFixture(t, func(labels map[string]string) { labels[label] = "wrong-owner" })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			fixture.waitCounter(t, ctx, 0)
			source, err := NewWorkspaceSource()
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			for _, purpose := range []execution.CapturePurpose{execution.PublicationCapture, execution.TerminalCapture} {
				called := false
				err := source.ReadWorkspace(ctx, fixture.captureRequest(purpose), func(io.Reader) error { called = true; return nil })
				if !errors.Is(err, execution.ErrConflict) || called {
					t.Fatalf("mismatched %s capture: callback=%t error=%v", purpose, called, err)
				}
				fixture.assertState(t, ctx, true, false)
			}
		})
	}
}

func TestWorkspaceSourceDockerRejectsOmittedRuntimeProfile(t *testing.T) {
	fixture := newWorkspaceDockerFixture(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	fixture.waitCounter(t, ctx, 0)
	source, err := NewWorkspaceSource()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, omitted := range []string{"name", "version", "both"} {
		t.Run(omitted, func(t *testing.T) {
			request := fixture.captureRequest(execution.PublicationCapture)
			if omitted == "name" || omitted == "both" {
				request.Workspace.Key.RuntimeProfileName = ""
			}
			if omitted == "version" || omitted == "both" {
				request.Workspace.Key.RuntimeProfileVersion = ""
			}
			called := false
			err := source.ReadWorkspace(ctx, request, func(io.Reader) error { called = true; return nil })
			if err == nil || called {
				t.Errorf("omitted Runtime Profile %s accepted: callback=%t error=%v", omitted, called, err)
			}
			fixture.assertState(t, ctx, true, false)
		})
	}
}

func TestWorkspaceSourceDockerStopKeepsUnresolvedCreateRevoked(t *testing.T) {
	fixture := newWorkspaceDockerFixture(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ledger := newExecutionTestLedger()
	key := fixture.key
	key.AgentTurnID = uuid.NewString()
	digest := strings.Repeat("a", 64)
	if _, _, err := ledger.ReserveExecution(ctx, key, digest); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
		t.Fatal(err)
	}
	cleaner, err := NewExactLabelCleaner(ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
	if err != nil {
		t.Fatal(err)
	}
	defer cleaner.Close()
	if err := cleaner.EnsureKeyStopped(ctx, key); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("unresolved create stop = %v", err)
	}
	reservation := ledger.records[key]
	if !reservation.Revoked || reservation.CreationSettled || reservation.Resource != "" {
		t.Fatalf("empty inventory settled ambiguous creation: %#v", reservation)
	}
	if _, _, err := ledger.ReserveExecution(ctx, key, digest); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("unresolved stopped epoch admitted retry: %v", err)
	}
	// A neighboring managed execution is not stopped by the failed exact-key
	// recovery attempt. The fixture is the only daemon sentinel we inspect.
	fixture.assertState(t, ctx, true, false)
	fixture.waitCounter(t, ctx, 0)
}

func TestWorkspaceSourceDockerStopRejectsConflictingCreate(t *testing.T) {
	for _, label := range []string{executionDigestLabel, RuntimeProcessAssignmentLabel, RuntimeProcessMarkerLabel} {
		t.Run(label, func(t *testing.T) {
			fixture := newWorkspaceDockerFixture(t, func(labels map[string]string) { labels[label] = "conflicting-create" })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			fixture.waitCounter(t, ctx, 0)
			ledger := newExecutionTestLedger()
			fixture.reserveMain(t, ctx, ledger)
			cleaner, err := NewExactLabelCleaner(ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: ledger})
			if err != nil {
				t.Fatal(err)
			}
			defer cleaner.Close()
			if err := cleaner.EnsureKeyStopped(ctx, fixture.key); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("conflicting create received stop receipt: %v", err)
			}
			reservation := ledger.records[fixture.key]
			if !reservation.Revoked || reservation.CreationSettled || reservation.Resource != "" {
				t.Fatalf("conflicting create adopted during stop: %#v", reservation)
			}
			fixture.assertState(t, ctx, true, false)
		})
	}
}

type workspaceDockerFixture struct {
	client                           *mobyclient.Client
	key                              execution.Key
	id, volume, head, digest, remote string
}

func newWorkspaceDockerFixture(t *testing.T, mutateLabels func(map[string]string)) *workspaceDockerFixture {
	t.Helper()
	if os.Getenv("DOCKER_HOST") != "" {
		t.Fatal("workspace integration fixtures require local Docker with DOCKER_HOST unset")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	image, err := client.ImageInspect(ctx, "omnigrex/pi:1.4.0")
	if err != nil {
		t.Fatalf("inspect pinned Pi fixture image (mise run pi-image): %v", err)
	}
	fixture := &workspaceDockerFixture{client: client, key: executionTestRequest().Key, digest: strings.Repeat("a", 64)}
	fixture.key.AssignmentID, fixture.key.AgentSessionID, fixture.key.AgentTurnID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	fixture.key.ExecutionEpoch = 1
	fixture.volume = "omnigrex-workspace-capture-test-" + fixture.key.AgentTurnID
	if _, err := client.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{Name: fixture.volume}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if fixture.id != "" {
			if _, err := client.ContainerRemove(cleanupCtx, fixture.id, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
				t.Errorf("remove fixture container: %v", err)
			}
		}
		if _, err := client.VolumeRemove(cleanupCtx, fixture.volume, mobyclient.VolumeRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("remove fixture volume: %v", err)
		}
	})
	labels := keyLabels(fixture.key)
	labels[RuntimeProcessMarkerLabel], labels[executionDigestLabel] = RuntimeProcessMarkerValue, fixture.digest
	if mutateLabels != nil {
		mutateLabels(labels)
	}
	pids := int64(32)
	created, err := client.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Name:       "omnigrex-turn-" + fixture.key.AgentTurnID + "-epoch-1",
		Config:     &container.Config{Image: image.ID, User: "10001:10001", WorkingDir: "/workspace", Entrypoint: []string{"/bin/sh", "-ec"}, Cmd: []string{workspaceFixtureCommand}, Labels: labels},
		HostConfig: &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Resources: container.Resources{Memory: 128 << 20, PidsLimit: &pids}, Tmpfs: map[string]string{"/tmp": "rw,size=4m"}, Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: fixture.volume, Target: "/workspace"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.id = created.ID
	seed := t.TempDir()
	workspaceFixtureGit(t, ctx, seed, "init", "--initial-branch=main", "--template=")
	for name, content := range map[string]string{"payload.txt": "committed seed\n", "executable.sh": "#!/bin/sh\nexit 0\n", ".gitignore": "racing*\nrequest.txt\nresponse\nignored/\nlarge.bin\n"} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(seed, "executable.sh"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("payload.txt", filepath.Join(seed, "relative-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside/not-readable", filepath.Join(seed, "outside-link")); err != nil {
		t.Fatal(err)
	}
	workspaceFixtureGit(t, ctx, seed, "add", ".")
	workspaceFixtureGit(t, ctx, seed, "-c", "user.name=Workspace Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "seed")
	fixture.head = strings.TrimSpace(workspaceFixtureGit(t, ctx, seed, "rev-parse", "HEAD"))
	fixture.remote = filepath.Join(t.TempDir(), "trusted.git")
	workspaceFixtureGit(t, ctx, seed, "clone", "--bare", "--no-hardlinks", "--", seed, fixture.remote)
	if err := os.MkdirAll(filepath.Join(seed, "ignored", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "ignored", "private.txt"), []byte("ignored inventory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "large.bin"), bytes.Repeat([]byte("bounded-transfer\n"), (8<<20)/17), 0o640); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	transferred := make(chan error, 1)
	go func() {
		err := streamWorkspaceFixtureSeed(seed, writer)
		_ = writer.CloseWithError(err)
		transferred <- err
	}()
	_, copyErr := client.CopyToContainer(ctx, fixture.id, mobyclient.CopyToContainerOptions{DestinationPath: "/workspace", Content: reader, CopyUIDGID: true})
	_ = reader.CloseWithError(copyErr)
	if err := errors.Join(copyErr, <-transferred); err != nil {
		t.Fatal(err)
	}
	// Only a stream crossed the execution boundary. Retain the independent bare
	// trusted seed remote, but delete this checkout before any capture.
	if err := os.RemoveAll(seed); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ContainerStart(ctx, fixture.id, mobyclient.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *workspaceDockerFixture) captureRequest(purpose execution.CapturePurpose) execution.CaptureRequest {
	return execution.CaptureRequest{Workspace: execution.WorkspaceRef{Key: fixture.key}, OperationID: fixture.key.AgentTurnID, Purpose: purpose}
}

func (fixture *workspaceDockerFixture) reserveMain(t *testing.T, ctx context.Context, ledger *executionTestLedger) {
	t.Helper()
	if _, _, err := ledger.ReserveExecution(ctx, fixture.key, fixture.digest); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginExecutionPhase(ctx, fixture.key, execution.Main); err != nil {
		t.Fatal(err)
	}
}

func (fixture *workspaceDockerFixture) assertState(t *testing.T, ctx context.Context, running, paused bool) {
	t.Helper()
	actual, err := fixture.client.ContainerInspect(ctx, fixture.id, mobyclient.ContainerInspectOptions{})
	if err != nil || actual.Container.State == nil {
		t.Fatalf("inspect fixture state: %v", err)
	}
	state := actual.Container.State
	if state.Running != running || state.Paused != paused || !running && state.Status != container.StateExited {
		t.Fatalf("fixture state = %#v; want running=%t paused=%t", state, running, paused)
	}
}

func (fixture *workspaceDockerFixture) readFile(t *testing.T, ctx context.Context, path string) string {
	t.Helper()
	archive, err := fixture.client.CopyFromContainer(ctx, fixture.id, mobyclient.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Content.Close()
	entries, err := readWorkspaceFixtureArchive(archive.Content, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(entries[filepath.Base(path)].content)
}

func (fixture *workspaceDockerFixture) waitCounter(t *testing.T, ctx context.Context, after int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		archive, err := fixture.client.CopyFromContainer(ctx, fixture.id, mobyclient.CopyFromContainerOptions{SourcePath: "/workspace/racing.txt"})
		if err == nil {
			entries, readErr := readWorkspaceFixtureArchive(archive.Content, "")
			_ = archive.Content.Close()
			counter, parseErr := strconv.Atoi(strings.TrimSpace(string(entries["racing.txt"].content)))
			if readErr == nil && parseErr == nil && counter > after {
				return
			}
		} else if !errdefs.IsNotFound(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background writer did not advance beyond %d: %v", after, ctx.Err())
}

func (fixture *workspaceDockerFixture) waitFile(t *testing.T, ctx context.Context, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if fixture.readFile(t, ctx, path) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("foreground request did not receive response: %v", ctx.Err())
}

func (fixture *workspaceDockerFixture) copyFile(t *testing.T, ctx context.Context, directory, name string, content []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Uid: 10001, Gid: 10001, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.client.CopyToContainer(ctx, fixture.id, mobyclient.CopyToContainerOptions{DestinationPath: directory, Content: &buffer, CopyUIDGID: true}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *workspaceDockerFixture) git(t *testing.T, ctx context.Context, arguments ...string) {
	t.Helper()
	created, err := fixture.client.ExecCreate(ctx, fixture.id, mobyclient.ExecCreateOptions{WorkingDir: "/workspace", AttachStdout: true, AttachStderr: true, Cmd: append([]string{"git"}, arguments...)})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := fixture.client.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	var output bytes.Buffer
	if _, err := stdcopy.StdCopy(&output, &output, attached.Reader); err != nil {
		t.Fatal(err)
	}
	inspected, err := fixture.client.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
	if err != nil || inspected.Running || inspected.ExitCode != 0 {
		t.Fatalf("guest git %v: running=%t exit=%d error=%v\n%s", arguments, inspected.Running, inspected.ExitCode, err, &output)
	}
}

func streamWorkspaceFixtureSeed(root string, destination io.Writer) error {
	writer := tar.NewWriter(destination)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == root {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.Uid, header.Gid = 10001, 10001
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(writer, file)
			return errors.Join(err, file.Close())
		}
		return nil
	})
	return errors.Join(err, writer.Close())
}

type workspaceFixtureEntry struct {
	kind    byte
	mode    int64
	link    string
	content []byte
}

func readWorkspaceFixtureArchive(source io.Reader, prefix string) (map[string]workspaceFixtureEntry, error) {
	content, err := io.ReadAll(io.LimitReader(source, workspaceFixtureArchiveLimit+1))
	if err != nil || len(content) > workspaceFixtureArchiveLimit {
		return nil, fmt.Errorf("fixture archive exceeded bounded transfer: bytes=%d error=%v", len(content), err)
	}
	reader := tar.NewReader(bytes.NewReader(content))
	entries := make(map[string]workspaceFixtureEntry)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, prefix), "/")
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, fmt.Errorf("duplicate archive entry %q", name)
		}
		entries[name] = workspaceFixtureEntry{kind: header.Typeflag, mode: header.Mode & 0o777, link: header.Linkname, content: data}
	}
}

func assertWorkspaceFixtureInventory(t *testing.T, entries map[string]workspaceFixtureEntry, head string) {
	t.Helper()
	for _, name := range []string{".git", ".git/objects", "ignored/empty"} {
		if entries[name].kind != tar.TypeDir {
			t.Errorf("missing directory %q", name)
		}
	}
	if string(entries[".git/HEAD"].content) != "ref: refs/heads/main\n" || strings.TrimSpace(string(entries[".git/refs/heads/main"].content)) != head || len(entries[".git/index"].content) == 0 || len(entries[".git/objects/"+head[:2]+"/"+head[2:]].content) == 0 {
		t.Error("capture lost Git HEAD, index, refs, or commit objects")
	}
	for _, expected := range []struct {
		name    string
		mode    int64
		content string
	}{
		{"payload.txt", 0o644, "committed seed\n"},
		{"executable.sh", 0o751, "#!/bin/sh\nexit 0\n"},
		{"ignored/private.txt", 0o600, "ignored inventory\n"},
	} {
		actual := entries[expected.name]
		if actual.kind != tar.TypeReg || actual.mode != expected.mode || string(actual.content) != expected.content {
			t.Errorf("captured %s: kind=%d mode=%o content=%q", expected.name, actual.kind, actual.mode, actual.content)
		}
	}
	for name, target := range map[string]string{"relative-link": "payload.txt", "outside-link": "/outside/not-readable"} {
		if actual := entries[name]; actual.kind != tar.TypeSymlink || actual.link != target || len(actual.content) != 0 {
			t.Errorf("symlink %q followed or changed: %#v", name, actual)
		}
	}
	if actual := entries["large.bin"]; actual.mode != 0o640 || !bytes.Equal(actual.content, bytes.Repeat([]byte("bounded-transfer\n"), (8<<20)/17)) {
		t.Error("bounded multi-megabyte file was truncated or changed")
	}
}

func openWorkspaceFixtureCapture(t *testing.T, ctx context.Context, lifecycle *workspace.Lifecycle, ref execution.CaptureRef) map[string]workspaceFixtureEntry {
	t.Helper()
	reader, err := lifecycle.OpenCapture(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	entries, err := readWorkspaceFixtureArchive(reader, "")
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func materializeWorkspaceFixturePublication(t *testing.T, ctx context.Context, directory string, entries map[string]workspaceFixtureEntry, head string) {
	t.Helper()
	for name, entry := range entries {
		if !strings.HasPrefix(name, ".git/") || entry.kind != tar.TypeReg {
			continue
		}
		if filepath.ToSlash(filepath.Clean(name)) != name || strings.Contains(name, "..") {
			t.Fatalf("unsafe fixture Git entry %q", name)
		}
		target := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, entry.content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspaceFixtureGit(t, ctx, directory, "fsck", "--full")
	workspaceFixtureGit(t, ctx, directory, "reset", "--hard", head)
}

func workspaceFixtureGit(t *testing.T, ctx context.Context, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
