//go:build integration

package docker

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestDefaultSnapshotDockerInstallsMidRequestAndPreservesWorkspace(t *testing.T) {
	fixture := newWorkspaceDockerFixture(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	fixture.waitCounter(t, ctx, 0)
	source, err := NewWorkspaceSource()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	capture := func() map[string]workspaceFixtureEntry {
		t.Helper()
		var entries map[string]workspaceFixtureEntry
		err := source.ReadWorkspace(ctx, fixture.captureRequest(execution.PublicationCapture), func(r io.Reader) error {
			var err error
			entries, err = readWorkspaceFixtureArchive(r, "workspace/")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	fixture.git(t, ctx, "config", "remote.origin.url", "https://untrusted.invalid/wrong.git")
	fixture.git(t, ctx, "config", "credential.helper", "!touch /workspace/credential-helper-used")
	before := capture()
	// Advance the trusted remote only after preparation. B is not in the guest.
	tree := strings.TrimSpace(workspaceFixtureGit(t, ctx, fixture.remote, "rev-parse", fixture.head+"^{tree}"))
	b := strings.TrimSpace(workspaceFixtureGit(t, ctx, fixture.remote, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-p", fixture.head, "-m", "new default"))
	workspaceFixtureGit(t, ctx, fixture.remote, "update-ref", "refs/heads/main", b)
	root := t.TempDir()
	acquirer, err := workspace.New(workspace.Options{WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "private"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/omnigrex/default/" + strings.Repeat("a", 64)
	snapshot, err := acquirer.AcquireSnapshot(ctx, fixture.remote, "", b, ref)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := source.InstallSnapshot(ctx, execution.WorkspaceRef{Key: fixture.key}, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	after := capture()
	if _, ok := after["credential-helper-used"]; ok {
		t.Fatal("installer executed agent-controlled credentials configuration")
	}
	for name, entry := range before {
		if strings.HasPrefix(name, "racing") {
			continue
		}
		if !reflect.DeepEqual(entry, after[name]) {
			t.Errorf("changed existing path %s", name)
		}
	}
	if got := strings.TrimSpace(fixture.git(t, ctx, "rev-parse", ref)); got != b {
		t.Fatalf("ref=%s want %s", got, b)
	}
	if got := strings.TrimSpace(fixture.git(t, ctx, "merge-base", "HEAD", ref)); got != fixture.head {
		t.Fatal("ancestry unavailable", got)
	}
	fixture.git(t, ctx, "merge-tree", "--write-tree", "HEAD", ref)
	counter, err := strconv.Atoi(strings.TrimSpace(string(after["racing.txt"].content)))
	if err != nil {
		t.Fatal(err)
	}
	fixture.waitCounter(t, ctx, counter)
	fixture.copyFile(t, ctx, "/workspace", "response", []byte("ok\n"))
	fixture.waitFile(t, ctx, "/workspace/request.txt", "delivered\n")
}
