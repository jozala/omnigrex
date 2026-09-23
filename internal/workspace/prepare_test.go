package workspace_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workspace"
)

func TestLifecyclePreparesExactCredentialFreeWorkspace(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)

	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Credential: "short-lived-secret", Revision: fixture.first,
	})
	if err != nil {
		t.Fatalf("PrepareWorkspace(first) error = %v", err)
	}
	if got := gitOutput(t, paths.Workspace, "rev-parse", "HEAD"); got != fixture.first {
		t.Errorf("HEAD = %q, want %q", got, fixture.first)
	}
	if got := gitOutput(t, paths.Workspace, "remote", "get-url", "origin"); got != fixture.remote {
		t.Errorf("origin = %q, want credential-free %q", got, fixture.remote)
	}
	gitConfig, err := os.ReadFile(filepath.Join(paths.Workspace, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"short-lived-secret",
		base64.StdEncoding.EncodeToString([]byte("x-access-token:short-lived-secret")),
	} {
		if strings.Contains(string(gitConfig), secret) {
			t.Errorf(".git/config exposed credential material %q", secret)
		}
	}
	if strings.Contains(string(gitConfig), "extraHeader") {
		t.Errorf(".git/config persisted HTTP authorization: %s", gitConfig)
	}
	if got := gitOutput(t, paths.Workspace, "config", "--get", "core.hooksPath"); got != "/dev/null" {
		t.Errorf("core.hooksPath = %q, want /dev/null", got)
	}

	if err := os.WriteFile(filepath.Join(paths.Workspace, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, "tracked.txt"), []byte("agent edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err = lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Credential: "replacement-secret", Revision: fixture.second,
	})
	if err != nil {
		t.Fatalf("PrepareWorkspace(second) error = %v", err)
	}
	if got := gitOutput(t, paths.Workspace, "rev-parse", "HEAD"); got != fixture.second {
		t.Errorf("HEAD = %q, want %q", got, fixture.second)
	}
	content, err := os.ReadFile(filepath.Join(paths.Workspace, "tracked.txt"))
	if err != nil || string(content) != "second\n" {
		t.Errorf("tracked.txt = %q, %v, want second revision", content, err)
	}
	if _, err := os.Stat(filepath.Join(paths.Workspace, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt stat error = %v, want not exist", err)
	}
}

func TestLifecycleReplacesReadOnlyAgentCacheWithoutFollowingSymlinks(t *testing.T) {
	fixture := newGitFixture(t)
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkout := workspace.Checkout{AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(paths.Workspace, ".cache", "gopath", "pkg", "mod", "golang.org", "x", "text@v0.29.0")
	file := filepath.Join(module, "unicode", "doc.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("cached"), 0o444); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.Mkdir(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "marker"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(paths.Workspace, ".cache", "external")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(paths.Workspace, 0o755)
		for _, directory := range []string{module, filepath.Dir(file)} {
			_ = os.Chmod(directory, 0o755)
		}
		_ = os.Chmod(external, 0o755)
	})
	for _, directory := range []string{filepath.Dir(file), module, external, paths.Workspace} {
		if err := os.Chmod(directory, 0o000); err != nil {
			t.Fatal(err)
		}
	}
	checkout.Revision = fixture.second
	replaced, err := lifecycle.PrepareWorkspace(context.Background(), checkout)
	if err != nil {
		t.Fatalf("PrepareWorkspace() replacing agent cache: %v", err)
	}
	if got := gitOutput(t, replaced.Workspace, "rev-parse", "HEAD"); got != fixture.second {
		t.Errorf("replaced workspace HEAD = %q, want %q", got, fixture.second)
	}
	if _, err := os.Lstat(filepath.Join(replaced.Workspace, ".cache")); !os.IsNotExist(err) {
		t.Errorf("agent cache survived replacement: %v", err)
	}
	info, err := os.Stat(external)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("external symlink target permissions = %v, %v, want 000", info, err)
	}
	if err := os.Chmod(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(external, "marker")); err != nil || string(content) != "outside" {
		t.Errorf("external marker = %q, %v", content, err)
	}
}

func TestStaleStagedCheckoutCannotReplaceSuccessorWorkspace(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	initial := workspace.Checkout{AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first, ExecutionEpoch: 1}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, "agent-edit.txt"), []byte("keep until promotion"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(filepath.Dir(paths.Workspace), "workspace-retired-0-abandoned"), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	staleCtx, cancelStale := context.WithCancel(context.Background())
	defer cancelStale()
	stale := initial
	stale.Revision = fixture.second
	stale.ExecutionEpoch = 2
	stale.Fence = func(ctx context.Context, promote func(context.Context) error) error {
		if content, err := os.ReadFile(filepath.Join(paths.Workspace, "agent-edit.txt")); err != nil || string(content) != "keep until promotion" {
			return fmt.Errorf("active workspace changed before promotion: %q, %v", content, err)
		}
		close(ready)
		<-release
		return promote(ctx)
	}
	result := make(chan error, 1)
	go func() {
		_, err := lifecycle.PrepareWorkspace(staleCtx, stale)
		result <- err
	}()
	select {
	case <-ready:
	case err := <-result:
		t.Fatalf("stale checkout did not reach promotion: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("stale checkout did not reach promotion")
	}
	cancelStale()
	successor := initial
	successor.ExecutionEpoch = 3
	successor.Fence = func(ctx context.Context, promote func(context.Context) error) error { return promote(ctx) }
	if _, err := lifecycle.PrepareWorkspace(context.Background(), successor); err != nil {
		t.Fatalf("successor PrepareWorkspace() error = %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("stale PrepareWorkspace() error = %v, want canceled fence", err)
	}
	if got := gitOutput(t, paths.Workspace, "rev-parse", "HEAD"); got != fixture.first {
		t.Fatalf("successor workspace HEAD = %q, want %q", got, fixture.first)
	}
	entries, err := os.ReadDir(filepath.Dir(paths.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "workspace" {
		t.Errorf("assignment workspace entries = %v, want only active workspace", entries)
	}
}

func TestWorkspacePromotionRequiresFenceCallback(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	checkout := workspace.Checkout{AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	checkout.Revision = fixture.second
	checkout.Fence = func(context.Context, func(context.Context) error) error { return nil }
	if _, err := lifecycle.PrepareWorkspace(context.Background(), checkout); err == nil {
		t.Fatal("PrepareWorkspace() accepted a fence that skipped promotion")
	}
	if got := gitOutput(t, paths.Workspace, "rev-parse", "HEAD"); got != fixture.first {
		t.Fatalf("skipped promotion changed active workspace to %q", got)
	}
}

func TestOlderTurnDoesNotCleanSuccessorRollbackDirectory(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	checkout := workspace.Checkout{AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first, ExecutionEpoch: 8}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(filepath.Dir(paths.Workspace), "workspace-retired-9-successor")
	if err := os.Mkdir(rollback, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, "prior-workspace"), []byte("restore on failure"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkout.Revision = fixture.second
	if _, err := lifecycle.PrepareWorkspace(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(rollback, "prior-workspace")); err != nil || string(content) != "restore on failure" {
		t.Fatalf("older turn removed successor rollback directory: %q, %v", content, err)
	}
}

func TestCheckoutRetriesFailedDetachedWorkspaceCleanup(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	checkout := workspace.Checkout{AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first, ExecutionEpoch: 7}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	checkout.Revision = fixture.second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checkout.Fence = func(ctx context.Context, promote func(context.Context) error) error {
		if err := promote(ctx); err != nil {
			return err
		}
		cancel()
		return nil
	}
	if _, err := lifecycle.PrepareWorkspace(ctx, checkout); !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareWorkspace() after unfinished old-tree cleanup = %v, want cancellation", err)
	}
	checkout.ExecutionEpoch = 8
	checkout.Fence = nil
	if _, err := lifecycle.PrepareWorkspace(context.Background(), checkout); err != nil {
		t.Fatalf("successor PrepareWorkspace() did not clean retired tree: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(paths.Workspace))
	if err != nil || len(entries) != 1 || entries[0].Name() != "workspace" {
		t.Fatalf("assignment directory after cleanup retry = %v, %v", entries, err)
	}
}

func TestCheckoutReportsOlderStagedWorkspaceCleanupBacklog(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	checkout := workspace.Checkout{AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first, ExecutionEpoch: 1}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		if err := os.Mkdir(filepath.Join(filepath.Dir(paths.Workspace), fmt.Sprintf("workspace-staged-1-abandoned-%d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	checkout.ExecutionEpoch = 2
	if _, err := lifecycle.PrepareWorkspace(context.Background(), checkout); err == nil {
		t.Fatal("PrepareWorkspace() hid abandoned staged workspace cleanup backlog")
	}
	if _, err := lifecycle.PrepareWorkspace(context.Background(), checkout); err != nil {
		t.Fatalf("PrepareWorkspace() after bounded cleanup retry = %v", err)
	}
}

func TestLifecycleNeverReturnsGitCredentialsInErrors(t *testing.T) {
	authorizations := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case authorizations <- request.Header.Get("Authorization"):
		default:
		}
		http.Error(writer, "denied", http.StatusUnauthorized)
	}))
	defer server.Close()
	credential := "top-secret-token-123"

	_, err := newLifecycle(t).PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: server.URL + "/repository.git", Credential: credential,
		Revision: strings.Repeat("a", 40),
	})
	if err == nil {
		t.Fatal("PrepareWorkspace() error = nil")
	}
	if strings.Contains(err.Error(), credential) {
		t.Errorf("error exposed credential: %v", err)
	}
	encodedCredential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + credential))
	if strings.Contains(err.Error(), encodedCredential) {
		t.Errorf("error exposed encoded credential: %v", err)
	}
	select {
	case authorization := <-authorizations:
		want := "Basic " + encodedCredential
		if authorization != want {
			t.Errorf("Git Authorization = %q, want %q", authorization, want)
		}
	default:
		t.Error("Git sent no HTTP request")
	}
}

type gitFixture struct {
	remote string
	first  string
	second string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, source, "init", "--initial-branch=main")
	gitRun(t, source, "config", "user.name", "Fixture")
	gitRun(t, source, "config", "user.email", "fixture@example.test")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "mise.toml"), []byte("[tools]\ntrusted = \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, source, "add", "tracked.txt", "mise.toml")
	gitRun(t, source, "commit", "-m", "first")
	first := gitOutput(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "mise.toml"), []byte("[tools]\npoison = \"latest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, source, "commit", "-am", "second")
	second := gitOutput(t, source, "rev-parse", "HEAD")
	remote := filepath.Join(root, "remote.git")
	gitRun(t, root, "clone", "--bare", source, remote)
	return gitFixture{remote: remote, first: first, second: second}
}

func newLifecycle(t *testing.T) *workspace.Lifecycle {
	t.Helper()
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return lifecycle
}

func gitRun(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func gitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
}

func quoted(value string) string {
	return fmt.Sprintf("%q", value)
}
