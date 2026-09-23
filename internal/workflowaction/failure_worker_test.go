package workflowaction

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

func TestFailureWorkerProcessesAtomicEscalation(t *testing.T) {
	fake := &failureWorkerStore{result: &store.WorkflowActionFailureEscalation{JobID: "escalation"}}
	worker, err := NewFailureWorker(fake, FailureWorkerConfig{
		ClaimOwner: "failure-worker", LeaseDuration: time.Second, IdlePollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed || fake.owner != "failure-worker" || fake.lease != time.Second {
		t.Fatalf("ProcessNext() = (%t, %v), owner %q lease %s", processed, err, fake.owner, fake.lease)
	}
}

func TestFailureWorkerReportsStoreFailureWithoutClaimLoop(t *testing.T) {
	want := errors.New("database unavailable")
	fake := &failureWorkerStore{err: want}
	worker, err := NewFailureWorker(fake, FailureWorkerConfig{
		ClaimOwner: "failure-worker", LeaseDuration: time.Second, IdlePollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessNext(context.Background())
	if processed || !errors.Is(err, want) || fake.calls != 1 {
		t.Fatalf("ProcessNext() = (%t, %v), calls %d", processed, err, fake.calls)
	}
}

type failureWorkerStore struct {
	result *store.WorkflowActionFailureEscalation
	err    error
	owner  string
	lease  time.Duration
	calls  int
}

func (fake *failureWorkerStore) ApplyNextWorkflowActionFailureEscalation(_ context.Context, owner string, lease time.Duration) (*store.WorkflowActionFailureEscalation, error) {
	fake.calls++
	fake.owner, fake.lease = owner, lease
	return fake.result, fake.err
}
