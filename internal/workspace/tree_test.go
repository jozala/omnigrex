package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jozala/omnigrex/internal/workspace"
)

func TestTreeSnapshotDetectsFilesystemChangesIndependentOfGitMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "file"), "content", 0o644)
	first, err := workspace.SnapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".git", "index"), "arbitrary index", 0o644)
	writeFile(t, filepath.Join(root, "nested", ".git", "metadata"), "nested Git metadata", 0o644)
	second, err := workspace.SnapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Error(".git metadata changed normalized snapshot")
	}
	if err := os.Chmod(filepath.Join(root, "file"), 0o755); err != nil {
		t.Fatal(err)
	}
	third, err := workspace.SnapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if second.Equal(third) {
		t.Error("executable-bit change was not detected")
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
