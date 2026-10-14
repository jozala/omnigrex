//go:build integration && microsandbox && cgo

package microsandbox

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/workspace"
)

// This exercises the trusted control-plane tools in the qualification image.
// The remote is generated without Git, so a missing Git fails inside Prepare,
// rather than being hidden by fixture setup or SDK protocol-only scenarios.
func TestNativeGitTrustedWorkspacePreparationAndInspection(t *testing.T) {
	if os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("requires native qualification image")
	}
	if os.Getuid() != 10001 || os.Getgid() != 10001 {
		t.Fatal("native qualification must run as UID/GID 10001")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	remote, seed, tip := nativeGitRemote(t, root)
	server := httptest.NewServer(http.FileServer(http.Dir(remote)))
	defer server.Close()
	request := testRequest()
	source := &nativeGitCaptureSource{}
	service, err := workspace.New(workspace.Options{
		WorkspaceRoot:   filepath.Join(root, "workspaces"),
		PublicationRoot: filepath.Join(root, "publications"),
		MiseRoot:        filepath.Join(root, "tools"),
		CaptureSource:   source,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := service.Prepare(ctx, workspace.Preparation{Key: request.Key, Checkout: workspace.Checkout{
		AssignmentID: request.Key.AssignmentID, ExecutionEpoch: request.Key.ExecutionEpoch,
		RepositoryURL: server.URL, Revision: seed,
	}})
	if err != nil {
		t.Fatalf("trusted workspace preparation: %v", err)
	}
	if ref.Key != request.Key || ref.Seed != seed {
		t.Fatalf("prepared identity/seed = %+v, want key %+v, seed %s", ref, request.Key, seed)
	}
	paths, err := service.Paths(request.Key.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if head := nativeGit(t, ctx, paths.Workspace, "rev-parse", "HEAD"); head != seed || head == tip {
		t.Fatalf("prepared HEAD = %s, want exact seed %s, not remote tip %s", head, seed, tip)
	}
	content, err := os.ReadFile(filepath.Join(paths.Workspace, "content.txt"))
	if err != nil || string(content) != "trusted seed\n" {
		t.Fatalf("prepared repository content = %q, %v", content, err)
	}
	if hooks := nativeGit(t, ctx, paths.Workspace, "config", "--local", "core.hooksPath"); hooks != "/dev/null" {
		t.Fatalf("prepared hooks path = %q", hooks)
	}
	if status := nativeGit(t, ctx, paths.Workspace, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("prepared workspace is dirty: %s", status)
	}
	if _, err := os.Stat(filepath.Join(paths.Workspace, "orchestrator-install-ran")); !os.IsNotExist(err) {
		t.Fatalf("repository installation hook ran in control plane: %v", err)
	}

	// Exercise sealed capture inspection and the real no-change publication path,
	// including trusted clone/fetch, commit objects, index and tree comparisons.
	source.directory = paths.Workspace
	capture := execution.CaptureRequest{Workspace: ref, OperationID: "native-git-packaging", Purpose: execution.PublicationCapture}
	result, err := service.Publish(ctx, workspace.Publication{
		AssignmentID: request.Key.AssignmentID, RepositoryURL: server.URL,
		BaseRevision: seed, Branch: "omnigrex/qualification", DefaultBranch: "main", Capture: capture,
	})
	if err != nil || result.Head != seed || result.Changed {
		t.Fatalf("unchanged publication = %+v, %v", result, err)
	}
	sealed, err := service.Capture(ctx, capture)
	if err != nil {
		t.Fatal(err)
	}
	for revision, want := range map[string]bool{seed: true, tip: false} {
		equal, err := service.CaptureTreesEqual(ctx, sealed, workspace.PublicationObservation{RepositoryURL: server.URL, Revision: revision})
		if err != nil || equal != want {
			t.Fatalf("capture comparison with %s = %t, %v; want %t", revision, equal, err, want)
		}
	}

	// Qualify the HTTPS helper and its dependency closure against local TLS,
	// trusting only the disposable fixture CA and keeping verification enabled.
	tlsServer := httptest.NewTLSServer(http.FileServer(http.Dir(remote)))
	defer tlsServer.Close()
	ca := filepath.Join(root, "fixture-ca.pem")
	nativeGitWrite(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))
	httpsCheckout := filepath.Join(root, "https-checkout")
	nativeGit(t, ctx, "", "-c", "http.sslCAInfo="+ca, "clone", "--", tlsServer.URL, httpsCheckout)
	if got := nativeGit(t, ctx, httpsCheckout, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("HTTPS checkout = %s, want %s", got, tip)
	}
	untrusted := exec.CommandContext(ctx, "git", "ls-remote", tlsServer.URL)
	untrusted.Env = nativeGitEnvironment()
	output, err := untrusted.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "server certificate verification failed") {
		t.Fatalf("HTTPS must reject untrusted fixture certificate: %s, %v", output, err)
	}
	certificates, err := x509.SystemCertPool()
	if err != nil || certificates == nil || len(certificates.Subjects()) == 0 {
		t.Fatalf("system HTTPS trust store unavailable: %v", err)
	}
	t.Log("PASS: non-root trusted preparation at exact seed, sealed capture comparison, no-change publication, HTTPS fetch and certificate verification")
}

// nativeGitRemote writes ordinary loose Git objects independently of installed
// Git, with two revisions so selecting the remote tip cannot satisfy the test.
func nativeGitRemote(t *testing.T, root string) (string, string, string) {
	t.Helper()
	remote := filepath.Join(root, "remote.git")
	object := func(kind string, content []byte) string {
		raw := append([]byte(fmt.Sprintf("%s %d\x00", kind, len(content))), content...)
		digest := sha1.Sum(raw)
		id := hex.EncodeToString(digest[:])
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		if _, err := writer.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		nativeGitWrite(t, filepath.Join(remote, "objects", id[:2], id[2:]), compressed.Bytes())
		return id
	}
	commit := func(content, parent string) string {
		var tree []byte
		for _, file := range []struct{ name, content string }{
			{"content.txt", content},
			{"mise.toml", "[hooks]\npostinstall = 'touch orchestrator-install-ran'\n"},
		} {
			blob, err := hex.DecodeString(object("blob", []byte(file.content)))
			if err != nil {
				t.Fatal(err)
			}
			tree = append(tree, []byte("100644 "+file.name+"\x00")...)
			tree = append(tree, blob...)
		}
		body := "tree " + object("tree", tree) + "\n"
		if parent != "" {
			body += "parent " + parent + "\n"
		}
		body += "author Fixture <fixture@omnigrex.invalid> 1700000000 +0000\ncommitter Fixture <fixture@omnigrex.invalid> 1700000000 +0000\n\nTrusted fixture\n"
		return object("commit", []byte(body))
	}
	seed := commit("trusted seed\n", "")
	tip := commit("later remote tip\n", seed)
	nativeGitWrite(t, filepath.Join(remote, "HEAD"), []byte("ref: refs/heads/main\n"))
	nativeGitWrite(t, filepath.Join(remote, "refs", "heads", "main"), []byte(tip+"\n"))
	nativeGitWrite(t, filepath.Join(remote, "info", "refs"), []byte(tip+"\trefs/heads/main\n"))
	nativeGitWrite(t, filepath.Join(remote, "objects", "info", "packs"), []byte("\n"))
	nativeGitWrite(t, filepath.Join(remote, "config"), []byte("[core]\nrepositoryformatversion = 0\nbare = true\n"))
	return remote, seed, tip
}

func nativeGitWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nativeGitEnvironment() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

func nativeGit(t *testing.T, ctx context.Context, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir, command.Env = directory, nativeGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s, %v", arguments, output, err)
	}
	return strings.TrimSpace(string(output))
}

// The packaging fixture has no concurrent writer and needs no VM or credentials.
// Use the image's real tar tool, then the production capture service validates it.
type nativeGitCaptureSource struct{ directory string }

func (source *nativeGitCaptureSource) ReadWorkspace(ctx context.Context, _ execution.CaptureRequest, receive func(io.Reader) error) error {
	command := exec.CommandContext(ctx, "tar", "-C", filepath.Dir(source.directory), "-cf", "-", filepath.Base(source.directory))
	content, err := command.Output()
	if err != nil {
		return err
	}
	return receive(bytes.NewReader(content))
}
