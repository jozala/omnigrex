package workspace_test

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/workspace"
	"golang.org/x/sys/unix"
)

func TestLifecyclePublishesCommittedWorkspaceFromCleanCheckout(t *testing.T) {
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
	gitRun(t, paths.Workspace, "config", "user.name", "Omnigrex Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@omnigrex.test")
	gitRun(t, paths.Workspace, "add", "-A")
	gitRun(t, paths.Workspace, "commit", "-m", "Apply deterministic change")
	wantHead := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")

	request := workspace.Publication{
		AssignmentID:  assignmentID,
		RepositoryURL: fixture.remote,
		BaseRevision:  fixture.second,
		Branch:        "omnigrex/feature",
		RecordProposedTip: func(_ context.Context, head string) error {
			if head != wantHead {
				t.Errorf("recorded proposed tip = %q, want %q", head, wantHead)
			}
			return nil
		},
	}
	result, err := lifecycle.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if !result.Changed || result.Head != wantHead {
		t.Fatalf("Publish() result = %#v", result)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/omnigrex/feature"); got != result.Head {
		t.Errorf("remote branch = %q, want %q", got, result.Head)
	}
	if got := gitOutput(t, fixture.remote, "show", "-s", "--format=%an|%ae|%cn|%ce", result.Head); got != "Omnigrex Developer|developer@omnigrex.test|Omnigrex Developer|developer@omnigrex.test" {
		t.Errorf("commit metadata = %q", got)
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

func TestLifecyclePublishesCommittedMergeHistoryWithoutFlattening(t *testing.T) {
	fixture := newGitFixture(t)
	gitRun(t, fixture.remote, "update-ref", "refs/heads/main", fixture.second)
	mainCheckout := filepath.Join(t.TempDir(), "main")
	gitRun(t, filepath.Dir(mainCheckout), "clone", fixture.remote, mainCheckout)
	gitRun(t, mainCheckout, "checkout", "main")
	gitRun(t, mainCheckout, "config", "user.name", "Main Contributor")
	gitRun(t, mainCheckout, "config", "user.email", "main@example.test")
	writeFile(t, filepath.Join(mainCheckout, "main.txt"), "main change\n", 0o644)
	gitRun(t, mainCheckout, "add", "main.txt")
	gitRun(t, mainCheckout, "commit", "-m", "Advance main")
	mainHead := gitOutput(t, mainCheckout, "rev-parse", "HEAD")
	gitRun(t, mainCheckout, "push", "origin", "HEAD:refs/heads/main")

	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, "developer.txt"), "first change\n", 0o644)
	gitRun(t, paths.Workspace, "add", "developer.txt")
	gitRun(t, paths.Workspace, "commit", "-m", "First developer commit")
	first := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(paths.Workspace, "developer.txt"), "second change\n", 0o644)
	gitRun(t, paths.Workspace, "commit", "-am", "Second developer commit")
	second := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	gitRun(t, paths.Workspace, "fetch", "origin", "main")
	gitRun(t, paths.Workspace, "merge", "--no-ff", "--no-edit", "FETCH_HEAD")
	merged := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	// A normal advance of main after the merge must not invalidate its verified historical parent.
	commitFixtureChange(t, fixture.remote, mainHead, "main", "main moved again\n", "Advance main again")

	result, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "omnigrex/feature", DefaultBranch: "main",
		RecordProposedTip: func(_ context.Context, head string) error {
			if head != merged {
				t.Errorf("recorded tip = %s, want %s", head, merged)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Head != merged || !result.Changed {
		t.Errorf("Publish() = %#v, want the exact merge commit %s", result, merged)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/omnigrex/feature"); got != merged {
		t.Errorf("remote head = %s, want local merge %s", got, merged)
	}
	if got := gitOutput(t, paths.Workspace, "rev-list", "--parents", "-n", "1", merged); got != merged+" "+second+" "+mainHead {
		t.Errorf("merge parents = %s", got)
	}
	if got := gitOutput(t, fixture.remote, "merge-base", "--is-ancestor", first, "refs/heads/omnigrex/feature"); got != "" {
		t.Errorf("first developer commit not reachable from published head")
	}
}

func TestLifecyclePublishesRepeatedVerifiedDefaultBranchMerges(t *testing.T) {
	fixture := newGitFixture(t)
	gitRun(t, fixture.remote, "update-ref", "refs/heads/main", fixture.second)
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	mainOne := commitFixtureChange(t, fixture.remote, fixture.second, "main", "main one\n", "First main advance")
	gitRun(t, paths.Workspace, "fetch", "origin", "main")
	gitRun(t, paths.Workspace, "merge", "--no-ff", "--no-edit", "FETCH_HEAD")
	mergeOne := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	mainTwo := commitFixtureChange(t, fixture.remote, mainOne, "main", "main two\n", "Second main advance")
	gitRun(t, paths.Workspace, "fetch", "origin", "main")
	gitRun(t, paths.Workspace, "merge", "--no-ff", "--no-edit", "FETCH_HEAD")
	mergeTwo := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	result, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "feature", DefaultBranch: "main", RecordProposedTip: func(_ context.Context, head string) error {
			if head != mergeTwo {
				t.Errorf("recorded tip = %s, want %s", head, mergeTwo)
			}
			return nil
		},
	})
	if err != nil || !result.Changed || result.Head != mergeTwo {
		t.Fatalf("Publish() = %#v, %v", result, err)
	}
	if got := gitOutput(t, fixture.remote, "rev-list", "--parents", "-n", "1", mergeOne); got != mergeOne+" "+fixture.second+" "+mainOne {
		t.Errorf("first merge parents = %s", got)
	}
	if got := gitOutput(t, fixture.remote, "rev-list", "--parents", "-n", "1", mergeTwo); got != mergeTwo+" "+mergeOne+" "+mainTwo {
		t.Errorf("second merge parents = %s", got)
	}
}

func TestLifecycleExtendsPreviouslyPublishedLegacyHead(t *testing.T) {
	fixture := newGitFixture(t)
	gitRun(t, fixture.remote, "update-ref", "refs/heads/main", fixture.second)
	legacy := commitFixtureChange(t, fixture.remote, fixture.second, "feature", "legacy\n",
		"Legacy publication\n\nOmnigrex-Operation-ID: old-operation")
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: legacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, "developer.txt"), "committed change\n", 0o644)
	gitRun(t, paths.Workspace, "add", "developer.txt")
	gitRun(t, paths.Workspace, "commit", "-m", "Extend legacy publication")
	proposed := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	result, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: legacy,
		ExpectedOldHead: legacy, Branch: "feature", DefaultBranch: "main",
		RecordProposedTip: func(context.Context, string) error { return nil },
	})
	if err != nil || !result.Changed || result.Head != proposed {
		t.Fatalf("Publish() = %#v, %v", result, err)
	}
	if got := gitOutput(t, fixture.remote, "rev-list", "--parents", "-n", "1", proposed); got != proposed+" "+legacy {
		t.Errorf("new commit parent = %s, want legacy head %s", got, legacy)
	}
}

func TestLifecyclePublicationRejectsRebasedAndNonDefaultMergeHistory(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, gitFixture, string)
	}{
		{name: "rewritten first parent", prepare: func(t *testing.T, fixture gitFixture, path string) {
			gitRun(t, path, "checkout", "--detach", fixture.first)
			writeFile(t, filepath.Join(path, "tracked.txt"), "rewritten\n", 0o644)
			gitRun(t, path, "commit", "-am", "Rewrite instead of extend")
		}},
		{name: "non-default merge", prepare: func(t *testing.T, fixture gitFixture, path string) {
			commitFixtureChange(t, fixture.remote, fixture.second, "other", "other branch\n", "Change other branch")
			gitRun(t, path, "fetch", "origin", "other")
			gitRun(t, path, "merge", "--no-ff", "--no-edit", "FETCH_HEAD")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			gitRun(t, fixture.remote, "update-ref", "refs/heads/main", fixture.second)
			lifecycle := newLifecycle(t)
			paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
			})
			if err != nil {
				t.Fatal(err)
			}
			gitRun(t, paths.Workspace, "config", "user.name", "Developer")
			gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
			test.prepare(t, fixture, paths.Workspace)
			_, err = lifecycle.Publish(context.Background(), workspace.Publication{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				DefaultBranch: "main", Branch: "feature", RecordProposedTip: func(context.Context, string) error {
					t.Fatal("unverified history must not be recorded")
					return nil
				},
			})
			if !errors.Is(err, workspace.ErrInvalidPublicationHistory) {
				t.Fatalf("Publish() = %v, want invalid history", err)
			}
			if got := gitOutput(t, fixture.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
				t.Errorf("invalid candidate was pushed: %s", got)
			}
		})
	}
}

func TestLifecyclePublicationRejectsAlteredLocalHistoryMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*testing.T, string, gitFixture)
	}{
		{name: "replace ref", alter: func(t *testing.T, path string, fixture gitFixture) {
			gitRun(t, path, "replace", "HEAD", fixture.first)
		}},
		{name: "graft", alter: func(t *testing.T, path string, fixture gitFixture) {
			writeFile(t, filepath.Join(path, ".git", "info", "grafts"), fixture.second+" "+fixture.first+"\n", 0o644)
		}},
		{name: "shallow", alter: func(t *testing.T, path string, fixture gitFixture) {
			writeFile(t, filepath.Join(path, ".git", "shallow"), fixture.second+"\n", 0o644)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			lifecycle := newLifecycle(t)
			paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
			})
			if err != nil {
				t.Fatal(err)
			}
			test.alter(t, paths.Workspace, fixture)
			_, err = lifecycle.Publish(context.Background(), workspace.Publication{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second, Branch: "feature",
				RecordProposedTip: func(context.Context, string) error {
					t.Fatal("altered history must never be recorded")
					return nil
				},
			})
			if !errors.Is(err, workspace.ErrInvalidPublicationHistory) {
				t.Fatalf("Publish() = %v, want invalid history", err)
			}
		})
	}
}

func TestLifecyclePublicationRejectsMalformedIntermediateTree(t *testing.T) {
	for _, test := range []struct {
		name string
		tree func(*testing.T, string, []byte) string
	}{
		{name: "duplicate files", tree: func(t *testing.T, path string, blob []byte) string {
			entry := append([]byte("100644 tracked.txt\x00"), blob...)
			return writeRawTree(t, path, append(append([]byte{}, entry...), entry...))
		}},
		{name: "unsorted files", tree: func(t *testing.T, path string, blob []byte) string {
			first := append([]byte("100644 tracked.txt\x00"), blob...)
			second := append([]byte("100644 earlier.txt\x00"), blob...)
			return writeRawTree(t, path, append(first, second...))
		}},
		{name: "noncanonical mode", tree: func(t *testing.T, path string, blob []byte) string {
			return writeRawTree(t, path, append([]byte("0100644 tracked.txt\x00"), blob...))
		}},
		{name: "duplicate nested files", tree: func(t *testing.T, path string, blob []byte) string {
			entry := append([]byte("100644 nested.txt\x00"), blob...)
			child, err := hex.DecodeString(writeRawTree(t, path, append(append([]byte{}, entry...), entry...)))
			if err != nil {
				t.Fatal(err)
			}
			return writeRawTree(t, path, append([]byte("40000 subdirectory\x00"), child...))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			lifecycle := newLifecycle(t)
			paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
			})
			if err != nil {
				t.Fatal(err)
			}
			blob, err := hex.DecodeString(gitOutput(t, paths.Workspace, "rev-parse", fixture.second+":tracked.txt"))
			if err != nil {
				t.Fatal(err)
			}
			badTree := test.tree(t, paths.Workspace, blob)
			gitRun(t, paths.Workspace, "config", "user.name", "Developer")
			gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
			intermediate := gitOutput(t, paths.Workspace, "commit-tree", badTree, "-p", fixture.second, "-m", "Malformed intermediate tree")
			baseTree := gitOutput(t, paths.Workspace, "rev-parse", fixture.second+"^{tree}")
			final := gitOutput(t, paths.Workspace, "commit-tree", baseTree, "-p", intermediate, "-m", "Restore valid final tree")
			gitRun(t, paths.Workspace, "checkout", "--detach", final)
			_, err = lifecycle.Publish(context.Background(), workspace.Publication{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				Branch: "feature", RecordProposedTip: func(context.Context, string) error {
					t.Error("malformed intermediate tree reached tip recording")
					return nil
				},
			})
			if !errors.Is(err, workspace.ErrInvalidPublicationHistory) {
				t.Fatalf("Publish() = %v, want invalid publication history", err)
			}
			if got := gitOutput(t, fixture.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
				t.Errorf("malformed history was pushed: %s", got)
			}
		})
	}
}

func writeRawTree(t *testing.T, repository string, raw []byte) string {
	t.Helper()
	command := exec.Command("git", "hash-object", "--literally", "-t", "tree", "-w", "--stdin")
	command.Dir, command.Stdin = repository, strings.NewReader(string(raw))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("write malformed tree: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestLifecyclePublicationChecksAssignedWorktreeDespiteCoreWorktreeOverride(t *testing.T) {
	for _, newCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "no new commits", true: "new commit"}[newCommit], func(t *testing.T) {
			fixture := newGitFixture(t)
			lifecycle := newLifecycle(t)
			paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if newCommit {
				gitRun(t, paths.Workspace, "config", "user.name", "Developer")
				gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
				writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "committed change\n", 0o644)
				gitRun(t, paths.Workspace, "commit", "-am", "Valid commit")
			}
			alternate := t.TempDir()
			gitRun(t, paths.Workspace, "--work-tree="+alternate, "checkout", "--force", "HEAD", "--", ".")
			gitRun(t, paths.Workspace, "config", "core.worktree", alternate)
			writeFile(t, filepath.Join(paths.Workspace, "not-ignored.txt"), "uncommitted\n", 0o644)
			if got := gitOutput(t, paths.Workspace, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
				t.Fatalf("fixture status = %q, want misleading clean status", got)
			}
			_, err = lifecycle.Publish(context.Background(), workspace.Publication{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				Branch: "feature", RecordProposedTip: func(context.Context, string) error {
					t.Error("dirty assigned worktree reached tip recording")
					return nil
				},
			})
			if !errors.Is(err, workspace.ErrUncommittedWorkspace) {
				t.Fatalf("Publish() = %v, want uncommitted workspace", err)
			}
		})
	}
}

func TestCommittedTreesEqualChecksAssignedWorktreeDespiteCoreWorktreeOverride(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "published\n", 0o644)
	gitRun(t, paths.Workspace, "commit", "-am", "Committed change")
	if _, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "feature", RecordProposedTip: func(context.Context, string) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	alternate := t.TempDir()
	gitRun(t, paths.Workspace, "--work-tree="+alternate, "checkout", "--force", "HEAD", "--", ".")
	gitRun(t, paths.Workspace, "config", "core.worktree", alternate)
	writeFile(t, filepath.Join(paths.Workspace, "not-ignored.txt"), "unpublished\n", 0o644)
	matching, err := workspace.CommittedTreesEqual(context.Background(), paths)
	if err != nil || matching {
		t.Fatalf("corroborate dirty assigned workspace = %t, %v", matching, err)
	}
}

func TestLifecyclePublicationRequiresFullyCommittedWorkspace(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, string)
	}{
		{name: "unstaged", change: func(t *testing.T, path string) {
			writeFile(t, filepath.Join(path, "tracked.txt"), "uncommitted\n", 0o644)
		}},
		{name: "staged", change: func(t *testing.T, path string) {
			writeFile(t, filepath.Join(path, "tracked.txt"), "staged\n", 0o644)
			gitRun(t, path, "add", "tracked.txt")
		}},
		{name: "untracked", change: func(t *testing.T, path string) {
			writeFile(t, filepath.Join(path, "untracked.txt"), "new\n", 0o644)
		}},
		{name: "unresolved", change: func(t *testing.T, path string) {
			gitRun(t, path, "config", "user.name", "Developer")
			gitRun(t, path, "config", "user.email", "developer@example.test")
			gitRun(t, path, "checkout", "-b", "conflicting")
			writeFile(t, filepath.Join(path, "tracked.txt"), "different branch\n", 0o644)
			gitRun(t, path, "commit", "-am", "Divergent change")
			gitRun(t, path, "checkout", "--detach", "HEAD~1")
			writeFile(t, filepath.Join(path, "tracked.txt"), "local change\n", 0o644)
			gitRun(t, path, "commit", "-am", "Local change")
			command := exec.Command("git", "merge", "--no-ff", "conflicting")
			command.Dir = path
			if err := command.Run(); err == nil {
				t.Fatal("fixture should produce unresolved merge entries")
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			lifecycle := newLifecycle(t)
			paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
			})
			if err != nil {
				t.Fatal(err)
			}
			test.change(t, paths.Workspace)
			_, err = lifecycle.Publish(context.Background(), workspace.Publication{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				Branch: "feature", RecordProposedTip: func(context.Context, string) error {
					t.Fatal("uncommitted tree must never record a proposed tip")
					return nil
				},
			})
			if !errors.Is(err, workspace.ErrUncommittedWorkspace) {
				t.Fatalf("Publish() = %v, want uncommitted workspace", err)
			}
		})
	}
}

func TestLifecyclePublicationWithNoNewCommitsDoesNotCreateBranch(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	if _, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second, Branch: "feature",
	})
	if err != nil || result.Changed || result.Head != fixture.second {
		t.Fatalf("no-op publication = %#v, %v", result, err)
	}
	if got := gitOutput(t, fixture.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
		t.Errorf("no-op created a branch: %s", got)
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "update-index", "--assume-unchanged", "tracked.txt")
	writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "hidden edit\n", 0o644)
	_, err = lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second, Branch: "feature",
	})
	if !errors.Is(err, workspace.ErrTreeMismatch) {
		t.Fatalf("no-op with hidden tracked edit = %v, want tree mismatch", err)
	}
}

func TestLifecycleNeverPushesWhenDurableTipRecordingFails(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "committed\n", 0o644)
	gitRun(t, paths.Workspace, "commit", "-am", "Commit before publishing")
	wantErr := errors.New("database unavailable")
	_, err = lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "feature", RecordProposedTip: func(context.Context, string) error { return wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v, want durable recording failure", err)
	}
	if got := gitOutput(t, fixture.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
		t.Errorf("branch moved despite unrecorded tip: %s", got)
	}
}

func TestLifecyclePublicationDetectsBranchChangeAfterRecordingTip(t *testing.T) {
	for _, branchExists := range []bool{false, true} {
		name := "new branch"
		if branchExists {
			name = "existing branch"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newGitFixture(t)
			if branchExists {
				gitRun(t, fixture.remote, "update-ref", "refs/heads/feature", fixture.second)
			}
			competing := commitFixtureChange(t, fixture.remote, fixture.second, "competing", "other publisher\n", "Competing publication")
			lifecycle := newLifecycle(t)
			paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
			})
			if err != nil {
				t.Fatal(err)
			}
			gitRun(t, paths.Workspace, "config", "user.name", "Developer")
			gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
			writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "agent change\n", 0o644)
			gitRun(t, paths.Workspace, "commit", "-am", "Proposed publication")
			proposed := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
			expectedOldHead := ""
			if branchExists {
				expectedOldHead = fixture.second
			}
			recorded := ""
			_, err = lifecycle.Publish(context.Background(), workspace.Publication{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				ExpectedOldHead: expectedOldHead, Branch: "feature",
				RecordProposedTip: func(_ context.Context, head string) error {
					recorded = head
					gitRun(t, fixture.remote, "update-ref", "refs/heads/feature", competing)
					return nil
				},
			})
			var mismatch *workspace.UnexpectedHeadError
			if !errors.As(err, &mismatch) || mismatch.Expected != expectedOldHead || mismatch.Actual != competing {
				t.Fatalf("Publish() error = %v, want recorded-tip head mismatch (%q, %q)", err, expectedOldHead, competing)
			}
			if recorded != proposed {
				t.Fatalf("recorded proposed tip = %q, want %q", recorded, proposed)
			}
			if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/feature"); got != competing {
				t.Fatalf("branch head after rejected publication = %q, want %q", got, competing)
			}
			reconciled, err := lifecycle.ReconcilePublication(context.Background(), workspace.PublicationReconciliation{
				AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
				ExpectedOldHead: expectedOldHead, ProposedRevision: recorded, HistoryPublication: true,
				Branch: "feature", OperationID: "recorded-tip-race",
			})
			if err != nil || reconciled.Outcome != workspace.PublicationReconciliationUnknown || reconciled.Head != competing {
				t.Fatalf("reconciliation after conflicting advance = %#v, %v", reconciled, err)
			}
		})
	}
}

func TestLifecyclePublicationLeaseRejectsBranchChangeAfterFinalHeadCheck(t *testing.T) {
	fixture := newGitFixture(t)
	gitRun(t, fixture.remote, "update-ref", "refs/heads/feature", fixture.second)
	competing := commitFixtureChange(t, fixture.remote, fixture.second, "competing", "other publisher\n", "Competing publication")
	gitExecutable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "push-attempted")
	wrapper := filepath.Join(t.TempDir(), "git-wrapper")
	writeExecutable(t, wrapper, "#!/bin/sh\n"+
		"if [ \"$1\" = push ]; then\n"+
		"  "+quoted(gitExecutable)+" --git-dir="+quoted(fixture.remote)+" update-ref refs/heads/feature "+quoted(competing)+" "+quoted(fixture.second)+" || exit 1\n"+
		"  : > "+quoted(marker)+"\n"+
		"fi\n"+
		"exec "+quoted(gitExecutable)+" \"$@\"\n")
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"),
		MiseRoot: filepath.Join(root, "mise"), GitExecutable: wrapper,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "agent change\n", 0o644)
	gitRun(t, paths.Workspace, "commit", "-am", "Proposed publication")
	proposed := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	recorded := ""
	_, err = lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		ExpectedOldHead: fixture.second, Branch: "feature",
		RecordProposedTip: func(_ context.Context, head string) error {
			recorded = head
			return nil
		},
	})
	if !errors.Is(err, workspace.ErrPushRejected) || !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("Publish() error = %v, want stale lease rejection", err)
	}
	if recorded != proposed {
		t.Fatalf("recorded proposed tip = %q, want %q", recorded, proposed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("push not attempted after tip recording: %v", err)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/feature"); got != competing {
		t.Fatalf("branch head after lease rejection = %q, want %q", got, competing)
	}
}

func TestLifecycleIgnoresAgentConfiguredUploadPackCommand(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "committed\n", 0o644)
	gitRun(t, paths.Workspace, "commit", "-am", "Committed change")
	marker := filepath.Join(t.TempDir(), "upload-pack-ran")
	gitRun(t, paths.Workspace, "config", "uploadpack.packObjectsHook", "touch "+marker+"; exit 7")
	result, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "feature", RecordProposedTip: func(context.Context, string) error { return nil },
	})
	if err != nil || !result.Changed {
		t.Fatalf("Publish() = %#v, %v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("agent-configured upload-pack command executed: %v", err)
	}
}

func TestLifecyclePublicationAllowsIgnoredFilesButDoesNotPublishThem(t *testing.T) {
	fixture := newGitFixture(t)
	lifecycle := newLifecycle(t)
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, paths.Workspace, "config", "user.name", "Developer")
	gitRun(t, paths.Workspace, "config", "user.email", "developer@example.test")
	writeFile(t, filepath.Join(paths.Workspace, ".gitignore"), "build/\n", 0o644)
	writeFile(t, filepath.Join(paths.Workspace, " leading.txt"), "preserve exact path\n", 0o644)
	writeFile(t, filepath.Join(paths.Workspace, "a.txt"), "sibling\n", 0o644)
	writeFile(t, filepath.Join(paths.Workspace, "a", "z"), "nested\n", 0o644)
	gitRun(t, paths.Workspace, "add", ".gitignore", " leading.txt", "a.txt", "a/z")
	gitRun(t, paths.Workspace, "commit", "-m", "Ignore local builds")
	if err := os.Mkdir(filepath.Join(paths.Workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(paths.Workspace, "build", "secret.log"), "never publish\n", 0o644)
	if err := unix.Mkfifo(filepath.Join(paths.Workspace, "build", "ignored-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := gitOutput(t, paths.Workspace, "rev-parse", "HEAD")
	result, err := lifecycle.Publish(context.Background(), workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, BaseRevision: fixture.second,
		Branch: "feature", RecordProposedTip: func(context.Context, string) error { return nil },
	})
	if err != nil || result.Head != want {
		t.Fatalf("Publish() = %#v, %v", result, err)
	}
	if got := gitOutput(t, fixture.remote, "ls-tree", "-r", "--name-only", want); strings.Contains(got, "secret.log") {
		t.Errorf("ignored local file was published: %s", got)
	}
	matching, err := workspace.CommittedTreesEqual(context.Background(), paths)
	if err != nil || !matching {
		t.Fatalf("ignored local file should not invalidate published tree = %t, %v", matching, err)
	}
	gitRun(t, paths.Workspace, "update-index", "--assume-unchanged", "tracked.txt")
	writeFile(t, filepath.Join(paths.Workspace, "tracked.txt"), "hidden tracked change\n", 0o644)
	matching, err = workspace.CommittedTreesEqual(context.Background(), paths)
	if err != nil || matching {
		t.Fatalf("hidden tracked edit must invalidate published tree = %t, %v", matching, err)
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
		ExpectedOldHead: fixture.first, Branch: "feature",
	})
	if !errors.Is(err, workspace.ErrUnexpectedHead) {
		t.Errorf("Publish() error = %v, want ErrUnexpectedHead", err)
	}
	var mismatch *workspace.UnexpectedHeadError
	if !errors.As(err, &mismatch) || mismatch.Expected != fixture.first || mismatch.Actual != fixture.second {
		t.Errorf("publication mismatch = %#v, error %v", mismatch, err)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/feature"); got != fixture.second {
		t.Errorf("remote feature head = %q, want unchanged %q", got, fixture.second)
	}
}
