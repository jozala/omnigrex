package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTreeOperationsDoNotFollowRegularFileReplacedBySymlink(t *testing.T) {
	for _, operation := range []string{"snapshot", "sync"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source")
			destination := filepath.Join(root, "destination")
			external := filepath.Join(root, "external-secret")
			if err := os.Mkdir(source, 0o755); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(source, "victim")
			if err := os.WriteFile(victim, []byte("safe"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(external, []byte("external secret"), 0o644); err != nil {
				t.Fatal(err)
			}

			replaced := false
			treeEntryInspected = func(currentOperation, path string) {
				if replaced || currentOperation != operation || path != victim {
					return
				}
				replaced = true
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, victim); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { treeEntryInspected = nil })

			var err error
			if operation == "snapshot" {
				_, err = SnapshotTree(source)
			} else {
				err = SyncTree(source, destination)
			}
			if !replaced {
				t.Fatal("test did not replace the classified regular file")
			}
			if err == nil {
				t.Fatal("tree operation followed replacement symlink, want error")
			}
			if content, readErr := os.ReadFile(filepath.Join(destination, "victim")); readErr == nil && string(content) == "external secret" {
				t.Fatal("external content leaked into destination")
			}
		})
	}
}
