package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workspace"
)

func TestLifecyclePublishesNormalizedWorkspaceFromCleanCheckout(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(paths.Workspace, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(paths.Workspace, "bin", "tool"), "#!/bin/sh\nexit 0\n", 0o755)
	if err := os.Symlink("bin/tool", filepath.Join(paths.Workspace, "tool")); err != nil {
		t.Fatal(err)
	}

	when := time.Date(2026, time.September, 3, 12, 34, 56, 0, time.FixedZone("ignored", 2*60*60))
	request := workspace.Publication{
		AssignmentID:  assignmentID,
		RepositoryURL: fixture.remote,
		BaseRevision:  fixture.second,
		Branch:        "omnigrex/feature",
		Message:       "Apply deterministic change",
		Identity:      workspace.CommitIdentity{Name: "Omnigrex Developer", Email: "developer@omnigrex.test"},
		Time:          when,
	}
	result, err := lifecycle.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if !result.Changed || result.Head == "" || result.Head == fixture.second {
		t.Fatalf("Publish() result = %#v", result)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/omnigrex/feature"); got != result.Head {
		t.Errorf("remote branch = %q, want %q", got, result.Head)
	}
	if got := gitOutput(t, paths.Publication, "show", "-s", "--format=%an|%ae|%cn|%ce|%aI|%cI", "HEAD"); got != "Omnigrex Developer|developer@omnigrex.test|Omnigrex Developer|developer@omnigrex.test|2026-09-03T10:34:56Z|2026-09-03T10:34:56Z" {
		t.Errorf("commit metadata = %q", got)
	}
	workspaceTree, err := workspace.SnapshotTree(paths.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	publicationTree, err := workspace.SnapshotTree(paths.Publication)
	if err != nil {
		t.Fatal(err)
	}
	if !workspaceTree.Equal(publicationTree) {
		t.Error("published tree differs from workspace tree")
	}

	gitRun(t, fixture.remote, "update-ref", "-d", "refs/heads/omnigrex/feature")
	repeated, err := lifecycle.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("repeated deterministic Publish() error = %v", err)
	}
	if repeated.Head != result.Head {
		t.Errorf("repeated commit = %q, want deterministic %q", repeated.Head, result.Head)
	}
}

func TestLifecycleRejectsUnexpectedOldHeadBeforePublishing(t *testing.T) {
	fixture := newGitFixture(t)
	gitRun(t, fixture.remote, "update-ref", "refs/heads/feature", fixture.second)
	lifecycle := newLifecycle(t)
	if _, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.first,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.first,
		ExpectedOldHead: fixture.first, Branch: "feature", Message: "must not publish",
		Identity: workspace.CommitIdentity{Name: "Developer", Email: "developer@example.test"}, Time: time.Unix(1_700_000_000, 0),
	})
	if !errors.Is(err, workspace.ErrUnexpectedHead) {
		t.Errorf("Publish() error = %v, want ErrUnexpectedHead", err)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/feature"); got != fixture.second {
		t.Errorf("remote feature head = %q, want unchanged %q", got, fixture.second)
	}
}
