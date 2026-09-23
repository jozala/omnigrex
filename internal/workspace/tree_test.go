package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jozala/omnigrex/internal/workspace"
)

func TestSyncTreeMirrorsPublishableFilesystemState(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	for _, directory := range []string{
		filepath.Join(source, ".git"), filepath.Join(source, "nested", ".git"), filepath.Join(destination, ".git"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(source, "regular.txt"), "regular\n", 0o644)
	writeFile(t, filepath.Join(source, "script.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(source, ".git", "workspace-index"), "private", 0o644)
	writeFile(t, filepath.Join(source, "nested", ".git", "metadata"), "private", 0o644)
	writeFile(t, filepath.Join(destination, ".git", "config"), "publication metadata", 0o644)
	writeFile(t, filepath.Join(destination, "deleted.txt"), "obsolete", 0o644)
	if err := os.Symlink("regular.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}

	before, err := workspace.SnapshotTree(source)
	if err != nil {
		t.Fatalf("SnapshotTree(source) error = %v", err)
	}
	if err := workspace.SyncTree(source, destination); err != nil {
		t.Fatalf("SyncTree() error = %v", err)
	}
	after, err := workspace.SnapshotTree(destination)
	if err != nil {
		t.Fatalf("SnapshotTree(destination) error = %v", err)
	}
	if !before.Equal(after) {
		t.Error("source and synchronized destination snapshots differ")
	}
	if _, err := os.Stat(filepath.Join(destination, "deleted.txt")); !os.IsNotExist(err) {
		t.Errorf("deleted.txt stat error = %v, want not exist", err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, ".git", "config")); err != nil || string(content) != "publication metadata" {
		t.Errorf("publication .git/config = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "nested", ".git")); !os.IsNotExist(err) {
		t.Errorf("nested .git stat error = %v, want not exist", err)
	}
	if info, err := os.Lstat(filepath.Join(destination, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link mode = %v, %v, want symlink", info, err)
	}
	if info, err := os.Stat(filepath.Join(destination, "script.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("script mode = %v, %v, want executable", info, err)
	}
}

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
