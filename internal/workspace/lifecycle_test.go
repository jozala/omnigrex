package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workspace"
)

const assignmentID = "12345678-1234-4234-8234-123456789abc"

func TestLifecycleResolvesIsolatedAssignmentPaths(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot:   filepath.Join(root, "workspaces"),
		PublicationRoot: filepath.Join(root, "publications"),
		MiseRoot:        filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatalf("Paths() error = %v", err)
	}
	if want := filepath.Join(root, "workspaces", "assignment-"+assignmentID, "workspace"); paths.Workspace != want {
		t.Errorf("Workspace = %q, want %q", paths.Workspace, want)
	}
	if want := filepath.Join(root, "publications", "assignment-"+assignmentID, "publication"); paths.Publication != want {
		t.Errorf("Publication = %q, want %q", paths.Publication, want)
	}
	if want := filepath.Join(root, "mise", "assignment-"+assignmentID, "mise"); paths.Mise != want {
		t.Errorf("Mise = %q, want %q", paths.Mise, want)
	}
}

func TestLifecycleOperationsRejectSymlinkedAssignmentMountPaths(t *testing.T) {
	fixture := newGitFixture(t)
	for _, testCase := range []struct {
		name      string
		operation string
		path      func(workspace.Paths) string
	}{
		{name: "workspace assignment", operation: "workspace", path: func(paths workspace.Paths) string { return filepath.Dir(paths.Workspace) }},
		{name: "workspace", operation: "workspace", path: func(paths workspace.Paths) string { return paths.Workspace }},
		{name: "mise assignment", operation: "mise", path: func(paths workspace.Paths) string { return filepath.Dir(paths.Mise) }},
		{name: "mise", operation: "mise", path: func(paths workspace.Paths) string { return paths.Mise }},
		{name: "publication assignment", operation: "publication", path: func(paths workspace.Paths) string { return filepath.Dir(paths.Publication) }},
		{name: "publication", operation: "publication", path: func(paths workspace.Paths) string { return paths.Publication }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			lifecycle, err := workspace.New(workspace.Options{
				WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"),
				MiseRoot: filepath.Join(root, "mise"), MiseExecutable: "false",
			})
			if err != nil {
				t.Fatal(err)
			}
			paths, err := lifecycle.Paths(assignmentID)
			if err != nil {
				t.Fatal(err)
			}
			unsafePath := testCase.path(paths)
			if err := os.MkdirAll(filepath.Dir(unsafePath), 0o755); err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(root, "external")
			if err := os.MkdirAll(filepath.Join(external, "workspace"), 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(external, "workspace", "marker")
			if err := os.WriteFile(marker, []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, unsafePath); err != nil {
				t.Fatal(err)
			}

			switch testCase.operation {
			case "workspace":
				_, err = lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
					AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first,
				})
			case "mise":
				_, err = lifecycle.ProvisionMise(context.Background(), workspace.MiseProvision{
					AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first,
				})
			case "publication":
				_, err = lifecycle.Publish(context.Background(), workspace.Publication{
					AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.first,
					Branch: "feature", Message: "publish", Identity: workspace.CommitIdentity{Name: "Agent", Email: "agent@example.test"},
					Time: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
				})
			}
			if !errors.Is(err, workspace.ErrUnsafeAssignmentPath) {
				t.Fatalf("Lifecycle operation error = %v, want ErrUnsafeAssignmentPath", err)
			}
			if content, readErr := os.ReadFile(marker); readErr != nil || string(content) != "outside" {
				t.Fatalf("external marker = %q, %v", content, readErr)
			}
		})
	}
}

func TestLifecycleDiscardsOwnedWorkspace(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, "review.txt"), []byte(strings.Repeat("change", 2)), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(paths.Workspace, ".cache", "gopath", "pkg", "mod")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "module.go"), []byte("cached"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(paths.Workspace, 0o755)
		_ = os.Chmod(cache, 0o755)
	})
	if err := os.Chmod(cache, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.Workspace, 0o000); err != nil {
		t.Fatal(err)
	}

	if err := lifecycle.DiscardWorkspace(assignmentID); err != nil {
		t.Fatalf("DiscardWorkspace() error = %v", err)
	}
	if _, err := os.Lstat(paths.Workspace); !os.IsNotExist(err) {
		t.Fatalf("discarded workspace stat error = %v, want not exist", err)
	}
}

func TestLifecycleDiscardRequiresFenceCallback(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.DiscardWorkspaceFenced(context.Background(), assignmentID, 0,
		func(context.Context, func(context.Context) error) error { return nil }); err == nil {
		t.Fatal("DiscardWorkspaceFenced() accepted a fence that skipped detachment")
	}
	if _, err := os.Stat(paths.Workspace); err != nil {
		t.Fatalf("skipped detachment removed workspace: %v", err)
	}
}

func TestLifecycleDiscardRetriesDetachedWorkspaceAfterCleanupFailure(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, "review.txt"), []byte("changes"), 0o644); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		fence := func(ctx context.Context, detach func(context.Context) error) error {
			if err := detach(ctx); err != nil {
				return err
			}
			cancel()
			return nil
		}
		if err := lifecycle.DiscardWorkspaceFenced(ctx, assignmentID, 7, fence); !errors.Is(err, context.Canceled) {
			t.Fatalf("discard attempt %d = %v, want unfinished cleanup", attempt, err)
		}
		cancel()
	}
	if _, err := os.Stat(paths.Workspace); !os.IsNotExist(err) {
		t.Fatalf("active workspace remains after detachment: %v", err)
	}
	if err := lifecycle.DiscardWorkspaceFenced(context.Background(), assignmentID, 7,
		func(ctx context.Context, detach func(context.Context) error) error { return detach(ctx) }); err != nil {
		t.Fatalf("retry DiscardWorkspaceFenced() error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(paths.Workspace))
	if err != nil || len(entries) != 0 {
		t.Fatalf("discarded assignment workspace entries = %v, %v, want none", entries, err)
	}
}

func TestLifecycleDiscardWorkspaceRejectsSymlinkedAssignment(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(paths.Workspace)), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(filepath.Join(external, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(external, "workspace", "marker")
	if err := os.WriteFile(marker, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Dir(paths.Workspace)); err != nil {
		t.Fatal(err)
	}

	if err := lifecycle.DiscardWorkspace(assignmentID); !errors.Is(err, workspace.ErrUnsafeAssignmentPath) {
		t.Fatalf("DiscardWorkspace() error = %v, want ErrUnsafeAssignmentPath", err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "outside" {
		t.Fatalf("external marker = %q, %v", content, err)
	}
}

func TestLifecycleRejectsUnsafeAssignmentIDsAndNestedPublication(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot:   filepath.Join(root, "shared"),
		PublicationRoot: filepath.Join(root, "shared", "assignment-"+assignmentID, "workspace"),
		MiseRoot:        filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := lifecycle.Paths(assignmentID); !errors.Is(err, workspace.ErrOverlappingPaths) {
		t.Errorf("nested publication error = %v, want ErrOverlappingPaths", err)
	}

	safe, err := workspace.New(workspace.Options{
		WorkspaceRoot: root, PublicationRoot: root, MiseRoot: root,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, value := range []string{"", ".", "../escape", "12345678-1234-4234-8234-123456789abg", "12345678123442348234123456789abc"} {
		t.Run(value, func(t *testing.T) {
			if _, err := safe.Paths(value); !errors.Is(err, workspace.ErrInvalidAssignmentID) {
				t.Errorf("Paths(%q) error = %v, want ErrInvalidAssignmentID", value, err)
			}
		})
	}
}
