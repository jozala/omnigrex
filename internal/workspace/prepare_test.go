package workspace_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
