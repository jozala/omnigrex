package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicationLockEntriesReturnToBaselineAfterSuccessAndErrors(t *testing.T) {
	lifecycle := newPublicationLockTestLifecycle(t)
	baselineRelease := lifecycle.acquirePublicationLock("baseline")
	baseline := publicationLockCount(lifecycle)

	wantErr := errors.New("operation failed")
	for index := range 1_000 {
		err := func() error {
			release := lifecycle.acquirePublicationLock(fmt.Sprintf("assignment-%d", index))
			defer release()
			if index%2 != 0 {
				return wantErr
			}
			return nil
		}()
		if (index%2 != 0) != errors.Is(err, wantErr) {
			t.Fatalf("operation %d error = %v", index, err)
		}
		if got := publicationLockCount(lifecycle); got != baseline {
			t.Fatalf("publication lock count after operation %d = %d, want baseline %d", index, got, baseline)
		}
	}

	baselineRelease()
	if got := publicationLockCount(lifecycle); got != 0 {
		t.Fatalf("publication lock count after releasing baseline = %d, want 0", got)
	}
}

func TestPublicationOperationsReleaseLockEntriesAfterSuccessAndErrors(t *testing.T) {
	lifecycle := newPublicationLockTestLifecycle(t)
	baseRevision := strings.Repeat("a", 40)
	repository := t.TempDir()
	fakeGit := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(fakeGit, []byte(`#!/bin/sh
for argument in "$@"; do
	destination=$argument
done
if [ "$1" = "clone" ]; then
	mkdir -p "$destination/.git"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	lifecycle.gitExecutable = fakeGit

	publishAssignment := "00000000-0000-4000-8000-000000000001"
	paths, err := lifecycle.Paths(publishAssignment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Publish(context.Background(), Publication{
		AssignmentID: publishAssignment, RepositoryURL: repository, BaseRevision: baseRevision,
		Branch: "feature", Message: "publish", Identity: CommitIdentity{Name: "Agent", Email: "agent@example.test"},
		Time: time.Unix(1_700_000_000, 0),
	}); err != nil {
		t.Fatalf("Publish() success-path error = %v", err)
	}
	if got := publicationLockCount(lifecycle); got != 0 {
		t.Fatalf("publication lock count after Publish() success = %d, want 0", got)
	}

	if _, err := lifecycle.ReconcilePublication(context.Background(), PublicationReconciliation{
		AssignmentID: "00000000-0000-4000-8000-000000000002", RepositoryURL: repository, BaseRevision: baseRevision,
		Branch: "feature", OperationID: "operation",
	}); err != nil {
		t.Fatalf("ReconcilePublication() success-path error = %v", err)
	}
	if got := publicationLockCount(lifecycle); got != 0 {
		t.Fatalf("publication lock count after ReconcilePublication() success = %d, want 0", got)
	}

	lifecycle.gitExecutable = filepath.Join(t.TempDir(), "git-does-not-exist")

	for index := range 100 {
		assignmentID := fmt.Sprintf("10000000-0000-4000-8000-%012x", index)
		var operationErr error
		if index%2 == 0 {
			_, operationErr = lifecycle.Publish(context.Background(), Publication{
				AssignmentID: assignmentID, RepositoryURL: repository, BaseRevision: baseRevision,
				Branch: "feature", Message: "publish", Identity: CommitIdentity{Name: "Agent", Email: "agent@example.test"},
				Time: time.Unix(1_700_000_000, 0),
			})
		} else {
			_, operationErr = lifecycle.ReconcilePublication(context.Background(), PublicationReconciliation{
				AssignmentID: assignmentID, RepositoryURL: repository, BaseRevision: baseRevision,
				Branch: "feature", OperationID: "operation",
			})
		}
		if operationErr == nil {
			t.Fatalf("operation %d error = nil", index)
		}
		if got := publicationLockCount(lifecycle); got != 0 {
			t.Fatalf("publication lock count after operation %d error = %d, want 0", index, got)
		}
	}
}

func TestPublicationLockCountsWaitersBeforeTheyBlock(t *testing.T) {
	lifecycle := newPublicationLockTestLifecycle(t)
	const assignmentID = "shared-assignment"
	holderRelease := lifecycle.acquirePublicationLock(assignmentID)

	lifecycle.locksMu.Lock()
	entry := lifecycle.publicationLocks[assignmentID]
	lifecycle.locksMu.Unlock()
	waiterAcquired := make(chan struct{})
	allowWaiterRelease := make(chan struct{})
	waiterDone := make(chan struct{})
	go func() {
		release := lifecycle.acquirePublicationLock(assignmentID)
		close(waiterAcquired)
		<-allowWaiterRelease
		release()
		close(waiterDone)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		lifecycle.locksMu.Lock()
		refs := entry.refs
		current := lifecycle.publicationLocks[assignmentID]
		lifecycle.locksMu.Unlock()
		if refs == 2 && current == entry {
			break
		}
		if time.Now().After(deadline) {
			holderRelease()
			close(allowWaiterRelease)
			<-waiterDone
			t.Fatal("blocked waiter was not counted")
		}
		runtime.Gosched()
	}

	holderRelease()
	select {
	case <-waiterAcquired:
	case <-time.After(5 * time.Second):
		close(allowWaiterRelease)
		<-waiterDone
		t.Fatal("waiter did not acquire publication lock")
	}
	lifecycle.locksMu.Lock()
	refs := entry.refs
	current := lifecycle.publicationLocks[assignmentID]
	lifecycle.locksMu.Unlock()
	if refs != 1 || current != entry {
		close(allowWaiterRelease)
		<-waiterDone
		t.Fatalf("publication lock entry after handoff = (%p, %d refs), want (%p, 1 ref)", current, refs, entry)
	}

	close(allowWaiterRelease)
	<-waiterDone
	if got := publicationLockCount(lifecycle); got != 0 {
		t.Fatalf("publication lock count after waiter release = %d, want 0", got)
	}
}

func TestPublicationLockMaintainsMutualExclusionDuringConcurrentChurn(t *testing.T) {
	lifecycle := newPublicationLockTestLifecycle(t)
	const (
		goroutines = 32
		iterations = 200
	)

	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	var active atomic.Int32
	var overlaps atomic.Int32
	for range goroutines {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			for range iterations {
				release := lifecycle.acquirePublicationLock("shared-assignment")
				if active.Add(1) != 1 {
					overlaps.Add(1)
				}
				runtime.Gosched()
				active.Add(-1)
				release()
			}
		}()
	}
	close(start)
	waitGroup.Wait()

	if got := overlaps.Load(); got != 0 {
		t.Fatalf("overlapping publication critical sections = %d, want 0", got)
	}
	if got := publicationLockCount(lifecycle); got != 0 {
		t.Fatalf("publication lock count after concurrent churn = %d, want 0", got)
	}
}

func newPublicationLockTestLifecycle(t *testing.T) *Lifecycle {
	t.Helper()
	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"),
		MiseRoot: filepath.Join(root, "mise"), GitExecutable: filepath.Join(root, "git-does-not-exist"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle
}

func publicationLockCount(lifecycle *Lifecycle) int {
	lifecycle.locksMu.Lock()
	defer lifecycle.locksMu.Unlock()
	return len(lifecycle.publicationLocks)
}
