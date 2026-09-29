package workspace_test

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/workspace"
)

func TestLifecycleReconcilesPublishedCommitByExactOperationTrailer(t *testing.T) {
	fixture := newGitFixture(t)
	const operationID = "github:publish:operation-17"
	published := commitFixtureChange(t, fixture.remote, fixture.second, "omnigrex/feature", "published\n", "Publish change\n\nOmnigrex-Operation-ID: "+operationID)
	lifecycle := newLifecycle(t)

	result, err := lifecycle.ReconcilePublication(context.Background(), workspace.PublicationReconciliation{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "omnigrex/feature", OperationID: operationID,
	})
	if err != nil {
		t.Fatalf("ReconcilePublication() error = %v", err)
	}
	if result.Outcome != workspace.PublicationReconciliationFound || result.Head != published {
		t.Errorf("ReconcilePublication() = %#v, want FOUND at %s", result, published)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, paths.Publication, "remote", "get-url", "origin"); got != fixture.remote {
		t.Errorf("publication origin = %q, want credential-free %q", got, fixture.remote)
	}
	if got := gitOutput(t, paths.Publication, "config", "--get", "core.hooksPath"); got != os.DevNull {
		t.Errorf("core.hooksPath = %q, want %q", got, os.DevNull)
	}
}

func TestLifecycleReconciliationDistinguishesAbsentAndConflictingAdvancement(t *testing.T) {
	fixture := newGitFixture(t)
	gitRun(t, fixture.remote, "update-ref", "refs/heads/existing", fixture.second)
	lifecycle := newLifecycle(t)
	input := workspace.PublicationReconciliation{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		ExpectedOldHead: fixture.second, Branch: "existing", OperationID: "github:publish:operation-18",
	}

	absent, err := lifecycle.ReconcilePublication(context.Background(), input)
	if err != nil {
		t.Fatalf("ReconcilePublication(absent) error = %v", err)
	}
	if absent.Outcome != workspace.PublicationReconciliationAbsent || absent.Head != fixture.second {
		t.Errorf("ReconcilePublication(absent) = %#v", absent)
	}

	advanced := commitFixtureChange(t, fixture.remote, fixture.second, "existing", "unrelated\n", "Unrelated branch advancement")
	unknown, err := lifecycle.ReconcilePublication(context.Background(), input)
	if err != nil {
		t.Fatalf("ReconcilePublication(advanced) error = %v", err)
	}
	if unknown.Outcome != workspace.PublicationReconciliationUnknown || unknown.Head != advanced {
		t.Errorf("ReconcilePublication(advanced) = %#v", unknown)
	}

	gitRun(t, fixture.remote, "update-ref", "-d", "refs/heads/existing")
	deleted, err := lifecycle.ReconcilePublication(context.Background(), input)
	if err != nil {
		t.Fatalf("ReconcilePublication(deleted) error = %v", err)
	}
	if deleted.Outcome != workspace.PublicationReconciliationUnknown || deleted.Head != "" {
		t.Errorf("ReconcilePublication(deleted) = %#v", deleted)
	}
}

func TestLifecycleReconciliationRequiresExactTrailerAndParent(t *testing.T) {
	tests := []struct {
		name    string
		parent  func(gitFixture) string
		message string
	}{
		{
			name:    "operation ID is only a body line",
			parent:  func(fixture gitFixture) string { return fixture.second },
			message: "Publish change\n\nOmnigrex-Operation-ID: github:publish:operation-19\nnot a trailer",
		},
		{
			name:    "operation ID differs",
			parent:  func(fixture gitFixture) string { return fixture.second },
			message: "Publish change\n\nOmnigrex-Operation-ID: github:publish:operation-190",
		},
		{
			name:    "parent differs",
			parent:  func(fixture gitFixture) string { return fixture.first },
			message: "Publish change\n\nOmnigrex-Operation-ID: github:publish:operation-19",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			advanced := commitFixtureChange(t, fixture.remote, test.parent(fixture), "conflict", "advanced\n", test.message)
			result, err := newLifecycle(t).ReconcilePublication(context.Background(), workspace.PublicationReconciliation{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				ExpectedOldHead: fixture.second, Branch: "conflict", OperationID: "github:publish:operation-19",
			})
			if err != nil {
				t.Fatalf("ReconcilePublication() error = %v", err)
			}
			if result.Outcome != workspace.PublicationReconciliationUnknown || result.Head != advanced {
				t.Errorf("ReconcilePublication() = %#v, want UNKNOWN at %s", result, advanced)
			}
		})
	}
}

func TestLifecycleReconciliationFindsOperationBelowCurrentBranchHead(t *testing.T) {
	fixture := newGitFixture(t)
	const operationID = "github:publish:operation-20"
	published := commitFixtureChange(t, fixture.remote, fixture.second, "descendant", "published\n", "Publish change\n\nOmnigrex-Operation-ID: "+operationID)
	descendant := commitFixtureChange(t, fixture.remote, published, "descendant", "later\n", "Later publication")

	result, err := newLifecycle(t).ReconcilePublication(context.Background(), workspace.PublicationReconciliation{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		ExpectedOldHead: fixture.second, Branch: "descendant", OperationID: operationID,
	})
	if err != nil {
		t.Fatalf("ReconcilePublication() error = %v", err)
	}
	if result.Outcome != workspace.PublicationReconciliationFound || result.Head != published || result.Head == descendant {
		t.Errorf("ReconcilePublication() = %#v, want FOUND at %s below %s", result, published, descendant)
	}
}

func TestLifecycleReconciliationValidatesEveryInputWithoutRunningGit(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
		GitExecutable: filepath.Join(root, "git-must-not-run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := workspace.PublicationReconciliation{
		AssignmentID: assignmentID, RepositoryURL: root, BaseRevision: strings.Repeat("a", 40),
		Branch: "feature", OperationID: "github:publish:operation-21",
	}
	tests := []struct {
		name   string
		mutate func(*workspace.PublicationReconciliation)
		cause  error
	}{
		{name: "assignment", mutate: func(input *workspace.PublicationReconciliation) { input.AssignmentID = "../escape" }, cause: workspace.ErrInvalidAssignmentID},
		{name: "remote", mutate: func(input *workspace.PublicationReconciliation) {
			input.RepositoryURL = "https://secret@example.test/repo.git"
		}, cause: workspace.ErrCredentialInRemote},
		{name: "credential", mutate: func(input *workspace.PublicationReconciliation) { input.Credential = "invalid credential" }, cause: workspace.ErrInvalidCredential},
		{name: "base", mutate: func(input *workspace.PublicationReconciliation) { input.BaseRevision = "main" }, cause: workspace.ErrInvalidRevision},
		{name: "expected head", mutate: func(input *workspace.PublicationReconciliation) { input.ExpectedOldHead = strings.Repeat("b", 40) }, cause: workspace.ErrInvalidPublicationReconciliation},
		{name: "branch", mutate: func(input *workspace.PublicationReconciliation) { input.Branch = "../feature" }, cause: workspace.ErrInvalidPublicationReconciliation},
		{name: "operation", mutate: func(input *workspace.PublicationReconciliation) { input.OperationID = "bad\noperation" }, cause: workspace.ErrInvalidPublicationReconciliation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			_, err := lifecycle.ReconcilePublication(context.Background(), input)
			if !errors.Is(err, test.cause) {
				t.Errorf("ReconcilePublication() error = %v, want %v", err, test.cause)
			}
		})
	}
}

func TestLifecycleReconciliationNeverReturnsCredentialsInErrors(t *testing.T) {
	root := t.TempDir()
	gitExecutable := filepath.Join(root, "git-failure")
	writeExecutable(t, gitExecutable, "#!/bin/sh\nprintf '%s\\n' \"$GIT_CONFIG_VALUE_2\" >&2\nexit 1\n")
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "mise"),
		GitExecutable: gitExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}
	const credential = "reconciliation-secret-token"

	_, err = lifecycle.ReconcilePublication(context.Background(), workspace.PublicationReconciliation{
		AssignmentID: assignmentID, RepositoryURL: root, Credential: credential, BaseRevision: strings.Repeat("a", 40),
		Branch: "feature", OperationID: "github:publish:operation-22",
	})
	if err == nil {
		t.Fatal("ReconcilePublication() error = nil")
	}
	if strings.Contains(err.Error(), credential) {
		t.Errorf("ReconcilePublication() error exposed credential: %v", err)
	}
	encodedCredential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + credential))
	if strings.Contains(err.Error(), encodedCredential) {
		t.Errorf("ReconcilePublication() error exposed encoded credential: %v", err)
	}
}

func TestLifecycleReconciliationRejectsSymlinkedPublicationAssignment(t *testing.T) {
	fixture := newGitFixture(t)
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
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(paths.Publication)), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(external, "marker")
	if err := os.WriteFile(marker, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Dir(paths.Publication)); err != nil {
		t.Fatal(err)
	}

	_, err = lifecycle.ReconcilePublication(context.Background(), workspace.PublicationReconciliation{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "feature", OperationID: "github:publish:operation-23",
	})
	if !errors.Is(err, workspace.ErrUnsafeAssignmentPath) {
		t.Fatalf("ReconcilePublication() error = %v, want ErrUnsafeAssignmentPath", err)
	}
	if content, readErr := os.ReadFile(marker); readErr != nil || string(content) != "outside" {
		t.Fatalf("external marker = %q, %v", content, readErr)
	}
}

func commitFixtureChange(t *testing.T, remote, parent, branch, content, message string) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	gitRun(t, filepath.Dir(repository), "clone", remote, repository)
	gitRun(t, repository, "config", "user.name", "Fixture")
	gitRun(t, repository, "config", "user.email", "fixture@example.test")
	gitRun(t, repository, "checkout", "--detach", parent)
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "commit", "-am", message)
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	gitRun(t, repository, "push", "origin", "HEAD:refs/heads/"+branch)
	return head
}
