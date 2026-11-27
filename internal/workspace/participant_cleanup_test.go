package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const cleanupID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func cleanupFixture(t *testing.T) (*Lifecycle, string) {
	t.Helper()
	root := t.TempDir()
	l, err := New(Options{WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"), MiseRoot: filepath.Join(root, "tools")})
	if err != nil {
		t.Fatal(err)
	}
	return l, root
}
func cleanupFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestParticipantCollectionAllStorageAndNeighbor(t *testing.T) {
	l, root := cleanupFixture(t)
	paths := []string{"workspaces/assignment-" + cleanupID + "/workspace", "workspaces/assignment-" + cleanupID + "/workspace-staged-1-abc", "workspaces/assignment-" + cleanupID + "/workspace-retired-1-abc", "publications/assignment-" + cleanupID + "/publication", "publications/assignment-" + cleanupID + "/.inspect-abc", "publications/assignment-" + cleanupID + "/.corroborate-abc", "tools/assignment-" + cleanupID + "/mise", "tools/assignment-" + cleanupID + "/tool-data", "workspaces/.captures/assignment-" + cleanupID + "/turn/.capture-stage"}
	for _, path := range paths {
		cleanupFile(t, filepath.Join(root, path, "file"))
	}
	neighbor := filepath.Join(root, "tools", "assignment-bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "mise", "file")
	cleanupFile(t, neighbor)
	readonly := filepath.Join(root, paths[6])
	if err := os.Chmod(readonly, 0); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	cleanupFile(t, filepath.Join(outside, "keep"))
	if err := os.Symlink(outside, filepath.Join(root, paths[7], "link")); err != nil {
		t.Fatal(err)
	}
	outcomes, err := l.CleanupParticipantStorage(context.Background(), cleanupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != len(paths)+1 {
		t.Fatalf("outcomes=%+v", outcomes)
	}
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("retained %s: %v", path, err)
		}
	}
	for _, path := range []string{neighbor, filepath.Join(outside, "keep")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
func TestParticipantCollectionMissingToolsDoesNotHidePublication(t *testing.T) {
	l, root := cleanupFixture(t)
	path := filepath.Join(root, "publications", "assignment-"+cleanupID, "publication")
	cleanupFile(t, filepath.Join(path, "file"))
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("publication remains")
	}
}
func TestParticipantCollectionRetryAfterPartialFailure(t *testing.T) {
	l, root := cleanupFixture(t)
	capture := filepath.Join(l.captureRoot, "assignment-"+cleanupID)
	cleanupFile(t, filepath.Join(capture, "file"))
	tool := filepath.Join(root, "tools", "assignment-"+cleanupID, "mise")
	outside := t.TempDir()
	cleanupFile(t, filepath.Join(outside, "keep"))
	if err := os.MkdirAll(filepath.Dir(tool), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tool); err != nil {
		t.Fatal(err)
	}
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); err == nil {
		t.Fatal("unsafe category succeeded")
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("expected partial progress")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	l2, err := New(Options{WorkspaceRoot: l.workspaceRoot, PublicationRoot: l.publicationRoot, MiseRoot: l.miseRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.CleanupAssignmentToolPaths(context.Background(), cleanupID); err != nil {
		t.Fatal(err)
	}
}
func TestParticipantCollectionRefusesUnknownAndAliasedRoot(t *testing.T) {
	l, root := cleanupFixture(t)
	unknown := filepath.Join(root, "tools", "assignment-"+cleanupID, "credentials", "keep")
	cleanupFile(t, unknown)
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); err == nil {
		t.Fatal("unknown ownership accepted")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal(err)
	}
	l.miseRoot = filepath.Join(l.workspaceRoot, "assignment-"+cleanupID, "workspace")
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); !errors.Is(err, ErrOverlappingPaths) {
		t.Fatalf("overlap=%v", err)
	}
}
func TestParticipantCollectionBoundedProgressCancellationAndAccounting(t *testing.T) {
	l, root := cleanupFixture(t)
	path := filepath.Join(root, "tools", "assignment-"+cleanupID, "mise")
	cleanupFile(t, filepath.Join(path, "file"))
	if err := os.Link(filepath.Join(path, "file"), filepath.Join(path, "hardlink")); err != nil {
		t.Fatal(err)
	}
	parent, err := openStorageRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	budget := 2
	var bytes int64
	if err := removeStorageTree(context.Background(), parent, "mise", &budget, &bytes); !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("budget=%v", err)
	}
	// Only one of the two links was removed, so no file blocks can be attributed yet.
	if bytes != 0 {
		t.Fatalf("hardlinks double-counted: %d", bytes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.CleanupAssignmentToolPaths(ctx, cleanupID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	outcomes, err := l.CleanupParticipantStorage(context.Background(), cleanupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 6 || outcomes[0].AllocatedBytes <= 0 || outcomes[0].Measurement == "" {
		t.Fatalf("measurement=%+v", outcomes)
	}
}
func TestParticipantCollectionCoincidentConfiguredRoots(t *testing.T) {
	root := t.TempDir()
	l, err := New(Options{WorkspaceRoot: root, PublicationRoot: root, MiseRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range []string{"workspace", "publication", "mise", "tool-data"} {
		cleanupFile(t, filepath.Join(root, "assignment-"+cleanupID, leaf, "file"))
	}
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); err != nil {
		t.Fatal(err)
	}
}

func TestParticipantCollectionWideStagingBacklogMakesProgress(t *testing.T) {
	l, root := cleanupFixture(t)
	for i := range 130 {
		cleanupFile(t, filepath.Join(root, "workspaces", "assignment-"+cleanupID, fmt.Sprintf("workspace-retired-1-%03d", i), "file"))
	}
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatalf("backlog=%v", err)
	}
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); err != nil {
		t.Fatalf("retry=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "workspaces", "assignment-"+cleanupID))
	if !os.IsNotExist(err) || len(entries) != 0 {
		t.Fatalf("remaining=%v %v", entries, err)
	}
}
func TestParticipantCollectionSparseFilesUseAllocatedBlocks(t *testing.T) {
	l, root := cleanupFixture(t)
	path := filepath.Join(root, "tools", "assignment-"+cleanupID, "mise", "sparse")
	cleanupFile(t, path)
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	file.Close()
	outcomes, err := l.CleanupParticipantStorage(context.Background(), cleanupID)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, result := range outcomes {
		total += result.AllocatedBytes
	}
	if total >= 1<<20 {
		t.Fatalf("counted sparse apparent size: %d", total)
	}
}
func TestStorageMountIdentityRefusesCrossMount(t *testing.T) {
	parent, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	foreign, err := os.Open("/proc")
	if err != nil {
		t.Skip("procfs unavailable")
	}
	defer foreign.Close()
	if err := storageSameMount(parent, foreign); !errors.Is(err, ErrUnsafeAssignmentPath) {
		t.Fatalf("foreign mount=%v", err)
	}
}

func TestParticipantCollectionRejectsCaptureRootAliasBeforeDeletion(t *testing.T) {
	l, root := cleanupFixture(t)
	l.captureRoot = l.miseRoot
	foreign := filepath.Join(root, "tools", "assignment-"+cleanupID, "credentials", "keep")
	cleanupFile(t, foreign)
	if err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID); !errors.Is(err, ErrOverlappingPaths) {
		t.Fatalf("capture alias=%v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal(err)
	}
}

func TestParticipantCollectionDeepTreeRetryAfterRestart(t *testing.T) {
	l, root := cleanupFixture(t)
	tree := filepath.Join(root, "tools", "assignment-"+cleanupID, "mise")
	deep := tree
	for range 96 {
		deep = filepath.Join(deep, "nested")
	}
	cleanupFile(t, filepath.Join(deep, "file"))
	outside := filepath.Join(root, "neighbor", "keep")
	cleanupFile(t, outside)
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(deep, "link")); err != nil {
		t.Fatal(err)
	}
	// A bounded attempt may stop, but must leave progress that a new Lifecycle
	// can discover without an in-memory traversal stack or weakened validation.
	for attempt := 0; attempt < 8; attempt++ {
		err := l.CleanupAssignmentToolPaths(context.Background(), cleanupID)
		if err == nil {
			if attempt == 0 {
				t.Fatal("deep fixture did not exercise a bounded retry")
			}
			if _, err := os.Lstat(tree); !os.IsNotExist(err) {
				t.Fatalf("tree remains: %v", err)
			}
			if content, err := os.ReadFile(outside); err != nil || string(content) != "retained" {
				t.Fatalf("neighbor changed: %q %v", content, err)
			}
			return
		}
		if !errors.Is(err, ErrCleanupIncomplete) {
			t.Fatal(err)
		}
		var newErr error
		l, newErr = New(Options{WorkspaceRoot: l.workspaceRoot, PublicationRoot: l.publicationRoot, MiseRoot: l.miseRoot})
		if newErr != nil {
			t.Fatal(newErr)
		}
	}
	t.Fatal("deep owned tree made no eventual progress across restarts")
}

func TestStorageCheckpointRejectsIdentityReplacement(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "tree"), 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := openStorageRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	original, err := openStorageChild(parent, "tree")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(original.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "tree"), filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	cleanupFile(t, filepath.Join(root, "tree", "keep"))
	if err := checkpointStorageDirectory(context.Background(), parent, "tree", parent, stat); !errors.Is(err, ErrUnsafeAssignmentPath) {
		t.Fatalf("identity replacement accepted: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "tree", "keep")); err != nil || string(content) != "retained" {
		t.Fatalf("replacement changed: %q %v", content, err)
	}
}

func TestStorageCheckpointRenameNeverReplacesDestination(t *testing.T) {
	root := t.TempDir()
	cleanupFile(t, filepath.Join(root, "source", "keep"))
	cleanupFile(t, filepath.Join(root, "destination", "keep"))
	parent, err := openStorageRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := storageRenameNoReplace(parent, "source", parent, "destination"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("destination replaced: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := storageRenameNoReplace(parent, "source", parent, "empty"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("empty destination replaced: %v", err)
	}
	if err := os.Symlink("destination", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := storageRenameNoReplace(parent, "source", parent, "alias"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("destination alias replaced: %v", err)
	}
	for _, name := range []string{"source", "destination"} {
		if content, err := os.ReadFile(filepath.Join(root, name, "keep")); err != nil || string(content) != "retained" {
			t.Fatalf("%s changed: %q %v", name, content, err)
		}
	}
}
