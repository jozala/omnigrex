package workspace

import (
	"bytes"
	"context"
	"github.com/jozala/omnigrex/internal/gitdelivery"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireDefaultSnapshotProvidesCompleteAncestryWithoutCredentials(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "remote")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(commandEnvironment("", ""), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, b, err)
		}
		return strings.TrimSpace(string(b))
	}
	git(repo, "init", "--initial-branch=trunk")
	git(repo, "commit", "--allow-empty", "-m", "A")
	a := git(repo, "rev-parse", "HEAD")
	git(repo, "commit", "--allow-empty", "-m", "B")
	b := git(repo, "rev-parse", "HEAD")
	lifecycle, err := New(Options{WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "private"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/omnigrex/default/" + strings.Repeat("b", 64)
	snapshot, err := lifecycle.AcquireSnapshot(context.Background(), repo, "fixture-secret", b, ref)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	git(target, "init")
	files, err := gitdelivery.Files(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(target, f.Name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Content, 0444); err != nil {
			t.Fatal(err)
		}
	}
	if got := git(target, "rev-parse", ref); got != b {
		t.Fatal(got)
	}
	if got := git(target, "merge-base", ref, a); got != a {
		t.Fatal(got)
	}
	git(target, "fsck", "--strict", "--full", ref)
	entries, err := os.ReadDir(lifecycle.publicationRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left acquisition state %v %v", entries, err)
	}
	for _, bad := range []string{strings.Repeat("f", 40), "HEAD", "--help"} {
		if _, err := lifecycle.AcquireSnapshot(context.Background(), repo, "fixture-secret", bad, ref); err == nil {
			t.Fatalf("accepted missing or arbitrary revision %q", bad)
		}
	}
}

func TestDefaultSnapshotDoesNotHideConflictsOrChangeLocalWork(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "remote")
	target := filepath.Join(root, "target")
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(commandEnvironment("", ""), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		b, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(b)), err
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		b, err := run(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %s %v", args, b, err)
		}
		return b
	}
	write := func(dir, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "conflict"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "--initial-branch=trunk")
	write(repo, "A\n")
	git(repo, "add", ".")
	git(repo, "commit", "-m", "A")
	a := git(repo, "rev-parse", "HEAD")
	git(repo, "clone", "--no-local", repo, target)
	write(repo, "B\n")
	git(repo, "commit", "-am", "B")
	b := git(repo, "rev-parse", "HEAD")
	write(target, "H\n")
	git(target, "commit", "-am", "H")
	h := git(target, "rev-parse", "HEAD")
	write(target, "dirty\n")
	before, err := os.ReadFile(filepath.Join(target, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(target, "cat-file", "-e", b+"^{commit}"); err == nil {
		t.Fatal("B already present before refresh")
	}
	lifecycle, err := New(Options{WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "private"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lifecycle.AcquireSnapshot(context.Background(), repo, "", b, "refs/omnigrex/default/"+strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	files, err := gitdelivery.Files(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(target, f.Name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Content, 0444); err != nil {
			t.Fatal(err)
		}
	}
	if got := git(target, "merge-base", h, snapshot.Ref); got != a {
		t.Fatal("wrong merge base", got)
	}
	if _, err := run(target, "merge-tree", "--write-tree", h, snapshot.Ref); err == nil {
		t.Fatal("conflict hidden by snapshot delivery")
	}
	if got := git(target, "rev-parse", "HEAD"); got != h {
		t.Fatal("changed local history")
	}
	after, err := os.ReadFile(filepath.Join(target, ".git/index"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("index changed", err)
	}
	dirty, err := os.ReadFile(filepath.Join(target, "conflict"))
	if err != nil || string(dirty) != "dirty\n" {
		t.Fatal("dirty work lost", err)
	}
}
