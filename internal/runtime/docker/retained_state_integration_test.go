//go:build integration

package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

// The adapter's efficient local access is exercised in a trusted control-plane
// helper with the real named volume mounted, including on Docker Desktop.
// Its Docker socket is never mounted into the disposable agent environments.
func TestRetainedStateDockerConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	const helperImage = "alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"
	run("pull", helperImage)
	agent := strings.Fields(run("image", "inspect", "omnigrex/pi:1.0.4", "--format", "{{.Id}} {{.Architecture}}"))
	if len(agent) != 2 {
		t.Fatal("incomplete agent image identity")
	}
	binary := filepath.Join(t.TempDir(), "state-tests")
	compile := exec.CommandContext(ctx, "go", "test", "-c", "-tags=integration", "-o", binary, ".")
	compile.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+agent[1], "CGO_ENABLED=0")
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("build control-plane conformance fixture: %v\n%s", err, output)
	}
	volume := fmt.Sprintf("omnigrex-state-conformance-%d", time.Now().UnixNano())
	run("volume", "create", volume)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "--force", volume).Run() })
	run("run", "--rm", "--network", "none", "--mount", "type=volume,src="+volume+",dst=/state", helperImage, "sh", "-ec", "chown 10001:10001 /state; chmod 0700 /state")
	socketGroup := run("run", "--rm", "--network", "none", "--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock,readonly", helperImage, "stat", "-c", "%g", "/var/run/docker.sock")
	id := run("create", "--network", "none", "--user", "10001:10001", "--group-add", socketGroup,
		"--mount", "type=volume,src="+volume+",dst=/state", "--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock,readonly",
		"--env", "OMNIGREX_STATE_CONFORMANCE_ROOT=/state", "--env", "OMNIGREX_STATE_CONFORMANCE_VOLUME="+volume,
		"--env", "OMNIGREX_STATE_AGENT_IMAGE="+agent[0], "--env", "OMNIGREX_STATE_AGENT_ARCH="+agent[1],
		"--entrypoint", "/state-tests", helperImage, "-test.run", "^TestRetainedStateMountedVolumeConformance$", "-test.v", "-test.timeout", "90s")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", id).Run() })
	run("cp", binary, id+":/state-tests")
	run("start", "--attach", id)
	if exit := run("inspect", id, "--format", "{{.State.ExitCode}}"); exit != "0" {
		t.Fatalf("mounted conformance exited %s", exit)
	}
}

func TestRetainedStateMountedVolumeConformance(t *testing.T) {
	root := os.Getenv("OMNIGREX_STATE_CONFORMANCE_ROOT")
	if root == "" {
		t.Skip("run by TestRetainedStateDockerConformance in its isolated control-plane helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	volume := os.Getenv("OMNIGREX_STATE_CONFORMANCE_VOLUME")
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", volume)
	ref.Image = os.Getenv("OMNIGREX_STATE_AGENT_IMAGE")
	source.refs[ref.ID] = ref
	states, err := NewRetainedState(root, volume, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	source.initialized[ref.ID] = true
	runtimeProfile, err := profile.NewPiV1("registry.example/omnigrex/pi@"+ref.Image, profile.Platform{OS: "linux", Arch: os.Getenv("OMNIGREX_STATE_AGENT_ARCH")})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Replace(ref.ID, "10000000", "20000000", 1)
	start := func(epoch int64, mustExist bool) (execution.Engine, error) {
		t.Helper()
		turnID := fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
		key := execution.Key{AssignmentID: ref.ID, AgentSessionID: sessionID, AgentTurnID: turnID, ExecutionEpoch: epoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
		request, err := (pi.TurnAdapter{}).BuildProcess(runtimeProfile, key, "", 512<<20, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Image = ref.Image // exact locally qualified fixture image
		request.Command = []string{"/usr/local/bin/node", "--input-type=module", "-e", fmt.Sprintf(`
import { openDurableSession } from '/opt/omnigrex/pi/session-state.mjs';
import { writeFile } from 'node:fs/promises';
await writeFile('/run/pi-agent/auth.json', 'ephemeral-credential-sentinel', {mode: 0o600});
const { manager, disposition } = await openDurableSession({agentSessionId: %q, runtimeSessionId: %q, mustExist: %t});
if (manager.getSessionId() !== %q) throw new Error('wrong Session');
console.log(disposition);
setInterval(() => {}, 1000);
`, sessionID, sessionID, mustExist, sessionID)}
		request.State = ref
		request.State.SessionID = key.AgentSessionID
		var attachments []execution.Attachment
		for _, attachment := range request.Attachments {
			if attachment.Resource == "state" {
				attachments = append(attachments, attachment)
			} else {
				request.TemporaryDirectories = append(request.TemporaryDirectories, execution.TemporaryDirectory{Target: attachment.Target, SizeBytes: 64 << 20})
			}
		}
		request.Attachments = attachments
		factory := ExecutionFactory{Network: "none", RuntimeStateVolume: volume, States: states}
		engine, err := factory.New(request, newExecutionTestLedger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = engine.EnsureAbsent(cleanupCtx)
			_ = engine.Close()
		})
		process, err := engine.Create(ctx, request, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Start(ctx); err != nil {
			t.Fatal(err)
		}
		stopRead := context.AfterFunc(ctx, func() { _ = process.Transport().Close() })
		defer stopRead()
		line, err := bufio.NewReader(process.Transport()).ReadString('\n')
		if err != nil {
			return engine, err
		}
		want := "created\n"
		if mustExist {
			want = "continued\n"
		}
		if line != want {
			return engine, fmt.Errorf("Session disposition = %q, want %q", line, want)
		}
		return engine, nil
	}
	firstEngine, err := start(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckStateRecoveryBoundary(ctx, volume); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("live writer recovery = %v", err)
	}
	if err := firstEngine.EnsureAbsent(ctx); err != nil {
		t.Fatal(err)
	}
	if err := CheckStateRecoveryBoundary(ctx, volume); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "assignment-"+ref.ID)
	readonly := filepath.Join(parent, "runtime-state", "readonly")
	if err := os.Mkdir(readonly, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "history.json"), []byte("retained history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0500); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive.Bytes(), []byte("ephemeral-credential-sentinel")) {
		t.Fatal("provider credentials entered retained state")
	}
	if err := os.Chmod(readonly, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "assignment-"+ref.ID, "runtime-state")); err != nil {
		t.Fatal(err)
	}
	if err := states.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := states.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 1 || entries[0].Name() != "runtime-state" {
			t.Fatalf("restore retry retained Session-data copies: %v, %v", entries, err)
		}
	}
	secondEngine, err := start(2, true)
	if err != nil {
		t.Fatalf("fresh environment could not continue exported/restored exact Session: %v", err)
	}
	if err := secondEngine.EnsureAbsent(ctx); err != nil {
		t.Fatal(err)
	}
	original := ref
	ref = source.add("10000000-0000-4000-8000-000000000002", volume)
	ref.Image = original.Image
	source.refs[ref.ID] = ref
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	source.initialized[ref.ID] = true
	neighborEngine, err := start(1, true)
	if err == nil {
		t.Fatal("another Participant continued the protected Session")
	}
	if err := neighborEngine.EnsureAbsent(ctx); err != nil {
		t.Fatal(err)
	}
	ref = original
	if err := states.Collect(ctx, ref); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("protected Session collected: %v", err)
	}
	source.collectable[ref.ID] = true
	interrupted := filepath.Join(parent, ".restore-"+strings.Repeat("a", 32))
	locked := filepath.Join(interrupted, "locked", "deeper")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "history.json"), []byte("interrupted restore history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(locked), 0000); err != nil {
		t.Fatal(err)
	}
	neighborCopy := filepath.Join(root, "assignment-10000000-0000-4000-8000-000000000002", ".restore-"+strings.Repeat("b", 32))
	if err := os.Mkdir(neighborCopy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(neighborCopy, "history.json"), []byte("neighbor history"), 0600); err != nil {
		t.Fatal(err)
	}
	states, err = states.WithCollector(AssignmentRuntimeStateCleanerOptions{
		Image: ref.Image, Platform: Platform{OS: "linux", Architecture: os.Getenv("OMNIGREX_STATE_AGENT_ARCH")}, User: "10001:10001", RuntimeStateVolume: volume,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer states.Close()
	for range 2 {
		if err := states.Collect(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("authorized collection retained Session-data copies: %v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(neighborCopy, "history.json")); err != nil {
		t.Fatalf("collection removed neighboring restore data: %v", err)
	}
	if err := states.Inspect(ctx, source.refs["10000000-0000-4000-8000-000000000002"]); err != nil {
		t.Fatalf("collection removed neighboring state: %v", err)
	}
}
