package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecyclePrepareWorkspaceRejectsWrongOwnedAssignmentDirectory(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		existing bool
	}{
		{name: "existing", existing: true},
		{name: "newly created", existing: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			const testAssignmentID = "12345678-1234-4234-8234-123456789abc"
			root := t.TempDir()
			lifecycle, err := New(Options{
				WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
				GitExecutable: filepath.Join(root, "git-must-not-run"),
			})
			if err != nil {
				t.Fatal(err)
			}
			paths, err := lifecycle.Paths(testAssignmentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(lifecycle.workspaceRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if testCase.existing {
				if err := os.Mkdir(filepath.Dir(paths.Workspace), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			originalDirectoryUID := directoryUID
			directoryUID = func(info os.FileInfo) (int, bool) {
				if info.Name() == filepath.Base(filepath.Dir(paths.Workspace)) {
					return os.Geteuid() + 1, true
				}
				return os.Geteuid(), true
			}
			t.Cleanup(func() { directoryUID = originalDirectoryUID })

			_, err = lifecycle.PrepareWorkspace(context.Background(), Checkout{
				AssignmentID: testAssignmentID, RepositoryURL: root, Revision: strings.Repeat("a", 40),
			})
			if !errors.Is(err, ErrUnsafeAssignmentPath) {
				t.Fatalf("PrepareWorkspace() error = %v, want ErrUnsafeAssignmentPath", err)
			}
		})
	}
}
