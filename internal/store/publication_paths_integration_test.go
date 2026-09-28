//go:build integration

package store_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jozala/omnigrex/internal/workspace"
)

func committedPublicationPaths(t *testing.T) workspace.Paths {
	t.Helper()
	root := t.TempDir()
	paths := workspace.Paths{Workspace: filepath.Join(root, "workspace"), Publication: filepath.Join(root, "publication")}
	if err := os.Mkdir(paths.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(directory string, arguments ...string) {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = directory
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	run(paths.Workspace, "init", "--quiet")
	run(paths.Workspace, "config", "user.name", "Fixture Developer")
	run(paths.Workspace, "config", "user.email", "developer@example.test")
	if err := os.WriteFile(filepath.Join(paths.Workspace, "tracked.txt"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(paths.Workspace, "add", "tracked.txt")
	run(paths.Workspace, "commit", "--quiet", "-m", "Published Change Proposal")
	run(root, "clone", "--quiet", paths.Workspace, paths.Publication)
	return paths
}
