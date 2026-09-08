package startup_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/startup"
	"github.com/jozala/omnigrex/internal/store"
)

func TestReconcilerRecoversExpiredTurnsBeforeLeavingTheirProcessesForRecoveryWorkers(t *testing.T) {
	expiredWithProcess := runtimeIdentity(1)
	expiredAbsent := runtimeIdentity(2)
	live := runtimeIdentity(3)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		expiredWithProcess: store.AgentTurnRuntimeExpired,
		expiredAbsent:      store.AgentTurnRuntimeExpired,
		live:               store.AgentTurnRuntimeLive,
	})
	runtimes := newFakeRuntimes(
		[]startup.ManagedRuntimeProcess{
			{Identity: expiredWithProcess, ContainerID: "expired"},
			{Identity: live, ContainerID: "live"},
		}, nil, nil,
	)
	reconciler := newReconciler(t, turns, runtimes)

	result, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RecoveredAgentTurns != 2 || result.RemovedRuntimeIdentities != 0 || result.Passes != 3 {
		t.Fatalf("Reconcile() result = %#v", result)
	}
	if turns.disposition(expiredWithProcess) != store.AgentTurnRuntimeRecovery ||
		turns.disposition(expiredAbsent) != store.AgentTurnRuntimeRecovery ||
		turns.disposition(live) != store.AgentTurnRuntimeLive {
		t.Fatalf("durable dispositions after recovery = %#v", turns.states)
	}
	if len(runtimes.ensureAbsentCalls) != 0 || len(runtimes.ensureMalformedCalls) != 0 {
		t.Fatalf("recovery/live Runtime Processes were cleaned directly: %#v %#v", runtimes.ensureAbsentCalls, runtimes.ensureMalformedCalls)
	}
}

func TestReconcilerRemovesTerminalUnknownActiveDuplicateAndMalformedProcesses(t *testing.T) {
	terminal := runtimeIdentity(10)
	active := runtimeIdentity(11)
	unknown := runtimeIdentity(12)
	duplicate := runtimeIdentity(13)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		terminal:  store.AgentTurnRuntimeTerminal,
		active:    store.AgentTurnRuntimeActive,
		duplicate: store.AgentTurnRuntimeTerminal,
	})
	runtimes := newFakeRuntimes(
		[]startup.ManagedRuntimeProcess{
			{Identity: terminal, ContainerID: "terminal"},
			{Identity: active, ContainerID: "active"},
			{Identity: unknown, ContainerID: "unknown"},
		},
		[]startup.DuplicateManagedRuntimeProcess{{Identity: duplicate, ContainerIDs: []string{"duplicate-a", "duplicate-b"}}},
		[]startup.MalformedManagedRuntimeProcess{{ContainerID: "malformed", Reason: "invalid_execution_epoch"}},
	)
	reconciler := newReconciler(t, turns, runtimes)

	result, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RemovedRuntimeIdentities != 4 || result.RemovedMalformedProcesses != 1 || result.Passes != 3 {
		t.Fatalf("Reconcile() result = %#v", result)
	}
	if runtimes.exactCalls(duplicate) != 1 {
		t.Fatalf("duplicate EnsureAbsent() calls = %d, want one exact cleanup", runtimes.exactCalls(duplicate))
	}
	if len(runtimes.processes) != 0 || len(runtimes.duplicates) != 0 || len(runtimes.malformed) != 0 {
		t.Fatalf("remaining Runtime Processes = %#v %#v %#v", runtimes.processes, runtimes.duplicates, runtimes.malformed)
	}
}

func TestReconcilerIdempotentRerunDoesNotAdoptOrCleanRecoveryProcess(t *testing.T) {
	identity := runtimeIdentity(20)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		identity: store.AgentTurnRuntimeRecovery,
	})
	runtimes := newFakeRuntimes([]startup.ManagedRuntimeProcess{{Identity: identity, ContainerID: "stale"}}, nil, nil)
	reconciler := newReconciler(t, turns, runtimes)

	for attempt := range 2 {
		result, err := reconciler.Reconcile(context.Background())
		if err != nil || result.RecoveredAgentTurns != 0 || result.RemovedRuntimeIdentities != 0 || result.Passes != 2 {
			t.Fatalf("Reconcile() rerun %d = (%#v, %v)", attempt, result, err)
		}
	}
	if len(runtimes.ensureAbsentCalls) != 0 {
		t.Fatalf("recovery Runtime Process cleanup calls = %#v", runtimes.ensureAbsentCalls)
	}
}

func TestReconcilerFencesLiveDuplicatesBeforeCleanupAndConverges(t *testing.T) {
	identity := runtimeIdentity(21)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		identity: store.AgentTurnRuntimeLive,
	})
	runtimes := newFakeRuntimes(nil, []startup.DuplicateManagedRuntimeProcess{{
		Identity: identity, ContainerIDs: []string{"live-a", "live-b"},
	}}, nil)
	reconciler := newReconciler(t, turns, runtimes)

	result, err := reconciler.Reconcile(context.Background())
	if err != nil || result.Passes != 3 || result.RemovedRuntimeIdentities != 1 {
		t.Fatalf("Reconcile() live duplicates = (%#v, %v), want converged cleanup", result, err)
	}
	if turns.disposition(identity) != store.AgentTurnRuntimeRecovery || turns.recoveryCount() != 1 ||
		runtimes.exactCalls(identity) != 1 || len(runtimes.duplicates) != 0 {
		t.Fatalf("live duplicate recovery = disposition %s, recoveries %d, cleanup calls %d, remaining %#v",
			turns.disposition(identity), turns.recoveryCount(), runtimes.exactCalls(identity), runtimes.duplicates)
	}
}

func TestReconcilerCleansEveryDuplicateUnderExistingRecoveryBarrier(t *testing.T) {
	identity := runtimeIdentity(22)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		identity: store.AgentTurnRuntimeRecovery,
	})
	runtimes := newFakeRuntimes(nil, []startup.DuplicateManagedRuntimeProcess{{
		Identity: identity, ContainerIDs: []string{"recovery-a", "recovery-b"},
	}}, nil)
	reconciler := newReconciler(t, turns, runtimes)

	result, err := reconciler.Reconcile(context.Background())
	if err != nil || result.RemovedRuntimeIdentities != 1 || runtimes.exactCalls(identity) != 1 ||
		len(runtimes.duplicates) != 0 || turns.recoveryCount() != 0 {
		t.Fatalf("Reconcile() recovery duplicates = (%#v, %v), cleanup calls %d, remaining %#v, recoveries %d",
			result, err, runtimes.exactCalls(identity), runtimes.duplicates, turns.recoveryCount())
	}
}

func TestReconcilerCleanupKeepsLiveIdentityThatDiffersOnlyByRuntimeProfile(t *testing.T) {
	live := runtimeIdentity(25)
	staleProfile := live
	staleProfile.RuntimeProfileVersion = "v0"
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		live: store.AgentTurnRuntimeLive,
	})
	runtimes := newFakeRuntimes([]startup.ManagedRuntimeProcess{
		{Identity: live, ContainerID: "live"},
		{Identity: staleProfile, ContainerID: "stale-profile"},
	}, nil, nil)
	reconciler := newReconciler(t, turns, runtimes)

	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(runtimes.processes) != 1 || runtimes.processes[0].Identity != live || runtimes.exactCalls(staleProfile) != 1 {
		t.Fatalf("remaining Runtime Processes = %#v, cleanup calls %#v", runtimes.processes, runtimes.ensureAbsentCalls)
	}
}

func TestConcurrentReconcilersConvergeWithoutCleaningLiveOrRecoveryProcesses(t *testing.T) {
	expired := runtimeIdentity(30)
	live := runtimeIdentity(31)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		expired: store.AgentTurnRuntimeExpired,
		live:    store.AgentTurnRuntimeLive,
	})
	runtimes := newFakeRuntimes([]startup.ManagedRuntimeProcess{
		{Identity: expired, ContainerID: "expired"},
		{Identity: live, ContainerID: "live"},
	}, nil, nil)
	reconcilers := []*startup.Reconciler{newReconciler(t, turns, runtimes), newReconciler(t, turns, runtimes)}
	start := make(chan struct{})
	errorsFound := make(chan error, len(reconcilers))
	var wait sync.WaitGroup
	for _, reconciler := range reconcilers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := reconciler.Reconcile(context.Background())
			errorsFound <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent Reconcile() error = %v", err)
		}
	}
	if turns.recoveries != 1 || turns.disposition(expired) != store.AgentTurnRuntimeRecovery {
		t.Fatalf("expired Turn recoveries = %d, disposition %s", turns.recoveries, turns.disposition(expired))
	}
	if len(runtimes.ensureAbsentCalls) != 0 {
		t.Fatalf("concurrent cleanup calls = %#v", runtimes.ensureAbsentCalls)
	}
}

func TestConcurrentReconcilersCleanRecoveryDuplicatesWithoutRemovingValidLiveProcess(t *testing.T) {
	duplicate := runtimeIdentity(32)
	live := runtimeIdentity(33)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		duplicate: store.AgentTurnRuntimeRecovery,
		live:      store.AgentTurnRuntimeLive,
	})
	runtimes := newFakeRuntimes([]startup.ManagedRuntimeProcess{{Identity: live, ContainerID: "live"}},
		[]startup.DuplicateManagedRuntimeProcess{{Identity: duplicate, ContainerIDs: []string{"recovery-a", "recovery-b"}}}, nil)
	reconcilers := []*startup.Reconciler{newReconciler(t, turns, runtimes), newReconciler(t, turns, runtimes)}
	start := make(chan struct{})
	errorsFound := make(chan error, len(reconcilers))
	var wait sync.WaitGroup
	for _, reconciler := range reconcilers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := reconciler.Reconcile(context.Background())
			errorsFound <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent Reconcile() error = %v", err)
		}
	}
	if runtimes.exactCalls(duplicate) == 0 || runtimes.exactCalls(live) != 0 ||
		len(runtimes.duplicates) != 0 || len(runtimes.processes) != 1 || runtimes.processes[0].Identity != live {
		t.Fatalf("concurrent cleanup calls duplicate/live = (%d, %d), remaining %#v %#v",
			runtimes.exactCalls(duplicate), runtimes.exactCalls(live), runtimes.duplicates, runtimes.processes)
	}
}

func TestReconcilerHonorsCancellationAndBoundsPersistentChurn(t *testing.T) {
	turns := newFakeTurnStore(nil)
	runtimes := newFakeRuntimes(nil, nil, nil)
	reconciler := newReconciler(t, turns, runtimes)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reconciler.Reconcile(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() canceled error = %v", err)
	}

	malformed := startup.MalformedManagedRuntimeProcess{ContainerID: "persistent", Reason: "incomplete_identity"}
	churning := &persistentMalformedRuntimes{malformed: malformed}
	bounded, err := startup.NewReconciler(turns, churning, churning, churning, startup.ReconcilerOptions{
		MaxPasses: 2, MaxRuntimeProcesses: 10, MaxRecoveries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := bounded.Reconcile(context.Background()); !errors.Is(err, startup.ErrReconciliationUnstable) || result.Passes != 2 {
		t.Fatalf("bounded Reconcile() = (%#v, %v)", result, err)
	}
}

func TestReconcilerAllowsExactRecoveryBoundAndDoesNotRecoverBeyondIt(t *testing.T) {
	first := runtimeIdentity(40)
	second := runtimeIdentity(41)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		first: store.AgentTurnRuntimeExpired,
	})
	runtimes := newFakeRuntimes(nil, nil, nil)
	exact, err := startup.NewReconciler(turns, runtimes, runtimes, runtimes, startup.ReconcilerOptions{
		MaxPasses: 4, MaxRuntimeProcesses: 10, MaxRecoveries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := exact.Reconcile(context.Background()); err != nil || result.RecoveredAgentTurns != 1 {
		t.Fatalf("exact-bound Reconcile() = (%#v, %v)", result, err)
	}

	turns = newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		first: store.AgentTurnRuntimeExpired, second: store.AgentTurnRuntimeExpired,
	})
	overflow, err := startup.NewReconciler(turns, runtimes, runtimes, runtimes, startup.ReconcilerOptions{
		MaxPasses: 4, MaxRuntimeProcesses: 10, MaxRecoveries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := overflow.Reconcile(context.Background())
	if !errors.Is(err, startup.ErrReconciliationUnstable) || result.RecoveredAgentTurns != 1 || turns.recoveries != 1 {
		t.Fatalf("overflow Reconcile() = (%#v, %v), durable recoveries %d", result, err, turns.recoveries)
	}
}

func TestMonitorLeavesLiveTurnUntouchedThenRecoversItAfterExpiry(t *testing.T) {
	identity := runtimeIdentity(50)
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		identity: store.AgentTurnRuntimeLive,
	})
	monitor, err := startup.NewMonitor(turns, startup.MonitorOptions{
		PollInterval: time.Millisecond, MaxRecoveriesPerPoll: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	if turns.disposition(identity) != store.AgentTurnRuntimeLive || turns.recoveryCount() != 0 {
		t.Fatalf("live Turn changed before expiry: %s with %d recoveries", turns.disposition(identity), turns.recoveryCount())
	}
	turns.mu.Lock()
	turns.states[identity] = store.AgentTurnRuntimeExpired
	turns.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for turns.disposition(identity) != store.AgentTurnRuntimeRecovery && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if turns.disposition(identity) != store.AgentTurnRuntimeRecovery || turns.recoveryCount() != 1 {
		t.Fatalf("expired Turn was not periodically recovered: %s with %d recoveries", turns.disposition(identity), turns.recoveryCount())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() cancellation error = %v", err)
	}
}

func TestConcurrentMonitorsRecoverEachExpiredTurnOnce(t *testing.T) {
	turns := newFakeTurnStore(map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition{
		runtimeIdentity(60): store.AgentTurnRuntimeExpired,
		runtimeIdentity(61): store.AgentTurnRuntimeExpired,
	})
	monitors := make([]*startup.Monitor, 2)
	for index := range monitors {
		monitor, err := startup.NewMonitor(turns, startup.MonitorOptions{
			PollInterval: time.Millisecond, MaxRecoveriesPerPoll: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		monitors[index] = monitor
	}
	start := make(chan struct{})
	results := make(chan error, len(monitors))
	for _, monitor := range monitors {
		go func() {
			<-start
			_, err := monitor.Reconcile(context.Background())
			results <- err
		}()
	}
	close(start)
	for range monitors {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if turns.recoveries != 2 {
		t.Fatalf("concurrent monitor recoveries = %d, want 2", turns.recoveries)
	}
}

func newReconciler(t *testing.T, turns startup.TurnStore, runtimes *fakeRuntimes) *startup.Reconciler {
	t.Helper()
	reconciler, err := startup.NewReconciler(turns, runtimes, runtimes, runtimes, startup.ReconcilerOptions{
		MaxPasses: 10, MaxRuntimeProcesses: 100, MaxRecoveries: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

func runtimeIdentity(number int) store.AgentTurnRuntimeIdentity {
	return store.AgentTurnRuntimeIdentity{
		AssignmentID:          fmt.Sprintf("10000000-0000-4000-8000-%012d", number),
		AgentSessionID:        fmt.Sprintf("20000000-0000-4000-8000-%012d", number),
		AgentTurnID:           fmt.Sprintf("30000000-0000-4000-8000-%012d", number),
		ExecutionEpoch:        int64(number + 1),
		RuntimeProfileName:    "opencode-acp",
		RuntimeProfileVersion: "v1",
	}
}

type fakeTurnStore struct {
	mu         sync.Mutex
	states     map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition
	recoveries int
}

func newFakeTurnStore(states map[store.AgentTurnRuntimeIdentity]store.AgentTurnRuntimeDisposition) *fakeTurnStore {
	return &fakeTurnStore{states: states}
}

func (turns *fakeTurnStore) ClaimAndRecoverExpiredAgentTurn(context.Context) (store.AgentTurnRecovery, bool, error) {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	for identity, disposition := range turns.states {
		if disposition == store.AgentTurnRuntimeExpired {
			turns.states[identity] = store.AgentTurnRuntimeRecovery
			turns.recoveries++
			return store.AgentTurnRecovery{TurnID: identity.AgentTurnID, ExecutionEpoch: identity.ExecutionEpoch}, true, nil
		}
	}
	return store.AgentTurnRecovery{}, false, nil
}

func (turns *fakeTurnStore) HasRecoverableExpiredAgentTurn(context.Context) (bool, error) {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	for _, disposition := range turns.states {
		if disposition == store.AgentTurnRuntimeExpired {
			return true, nil
		}
	}
	return false, nil
}

func (turns *fakeTurnStore) ClassifyAgentTurnRuntime(_ context.Context, identity store.AgentTurnRuntimeIdentity) (store.AgentTurnRuntimeState, bool, error) {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	disposition, found := turns.states[identity]
	return store.AgentTurnRuntimeState{Identity: identity, Disposition: disposition}, found, nil
}

func (turns *fakeTurnStore) FenceDuplicateAgentTurnRuntime(_ context.Context, identity store.AgentTurnRuntimeIdentity) (store.AgentTurnRecovery, error) {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	disposition, found := turns.states[identity]
	if !found {
		return store.AgentTurnRecovery{}, store.ErrAgentTurnFenceLost
	}
	if disposition == store.AgentTurnRuntimeRecovery {
		return store.AgentTurnRecovery{TurnID: identity.AgentTurnID, ExecutionEpoch: identity.ExecutionEpoch}, nil
	}
	if disposition != store.AgentTurnRuntimeLive {
		return store.AgentTurnRecovery{}, store.ErrAgentTurnFenceLost
	}
	turns.states[identity] = store.AgentTurnRuntimeRecovery
	turns.recoveries++
	return store.AgentTurnRecovery{TurnID: identity.AgentTurnID, ExecutionEpoch: identity.ExecutionEpoch}, nil
}

func (turns *fakeTurnStore) RecoverExpiredAgentTurn(_ context.Context, turnID string, epoch int64) (store.AgentTurnRecovery, error) {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	for identity, disposition := range turns.states {
		if identity.AgentTurnID != turnID || identity.ExecutionEpoch != epoch {
			continue
		}
		if disposition == store.AgentTurnRuntimeRecovery {
			return store.AgentTurnRecovery{TurnID: turnID, ExecutionEpoch: epoch}, nil
		}
		if disposition != store.AgentTurnRuntimeExpired {
			return store.AgentTurnRecovery{}, store.ErrAgentTurnNotExpired
		}
		turns.states[identity] = store.AgentTurnRuntimeRecovery
		turns.recoveries++
		return store.AgentTurnRecovery{TurnID: turnID, ExecutionEpoch: epoch}, nil
	}
	return store.AgentTurnRecovery{}, store.ErrAgentTurnFenceLost
}

func (turns *fakeTurnStore) disposition(identity store.AgentTurnRuntimeIdentity) store.AgentTurnRuntimeDisposition {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	return turns.states[identity]
}

func (turns *fakeTurnStore) recoveryCount() int {
	turns.mu.Lock()
	defer turns.mu.Unlock()
	return turns.recoveries
}

type fakeRuntimes struct {
	mu                   sync.Mutex
	processes            []startup.ManagedRuntimeProcess
	duplicates           []startup.DuplicateManagedRuntimeProcess
	malformed            []startup.MalformedManagedRuntimeProcess
	ensureAbsentCalls    []store.AgentTurnRuntimeIdentity
	ensureMalformedCalls []startup.MalformedManagedRuntimeProcess
}

func newFakeRuntimes(processes []startup.ManagedRuntimeProcess, duplicates []startup.DuplicateManagedRuntimeProcess, malformed []startup.MalformedManagedRuntimeProcess) *fakeRuntimes {
	return &fakeRuntimes{processes: processes, duplicates: duplicates, malformed: malformed}
}

func (runtimes *fakeRuntimes) List(context.Context) (startup.RuntimeInventorySnapshot, error) {
	runtimes.mu.Lock()
	defer runtimes.mu.Unlock()
	return startup.RuntimeInventorySnapshot{
		Processes:  append([]startup.ManagedRuntimeProcess(nil), runtimes.processes...),
		Duplicates: append([]startup.DuplicateManagedRuntimeProcess(nil), runtimes.duplicates...),
		Malformed:  append([]startup.MalformedManagedRuntimeProcess(nil), runtimes.malformed...),
	}, nil
}

func (runtimes *fakeRuntimes) EnsureAbsent(_ context.Context, identity store.AgentTurnRuntimeIdentity) error {
	runtimes.mu.Lock()
	defer runtimes.mu.Unlock()
	runtimes.ensureAbsentCalls = append(runtimes.ensureAbsentCalls, identity)
	processes := runtimes.processes[:0]
	for _, process := range runtimes.processes {
		if process.Identity != identity {
			processes = append(processes, process)
		}
	}
	runtimes.processes = processes
	duplicates := runtimes.duplicates[:0]
	for _, duplicate := range runtimes.duplicates {
		if duplicate.Identity != identity {
			duplicates = append(duplicates, duplicate)
		}
	}
	runtimes.duplicates = duplicates
	return nil
}

func (runtimes *fakeRuntimes) EnsureMalformedAbsent(_ context.Context, malformed startup.MalformedManagedRuntimeProcess) error {
	runtimes.mu.Lock()
	defer runtimes.mu.Unlock()
	runtimes.ensureMalformedCalls = append(runtimes.ensureMalformedCalls, malformed)
	remaining := runtimes.malformed[:0]
	for _, candidate := range runtimes.malformed {
		if candidate != malformed {
			remaining = append(remaining, candidate)
		}
	}
	runtimes.malformed = remaining
	return nil
}

func (runtimes *fakeRuntimes) exactCalls(identity store.AgentTurnRuntimeIdentity) int {
	runtimes.mu.Lock()
	defer runtimes.mu.Unlock()
	count := 0
	for _, candidate := range runtimes.ensureAbsentCalls {
		if candidate == identity {
			count++
		}
	}
	return count
}

type persistentMalformedRuntimes struct {
	malformed startup.MalformedManagedRuntimeProcess
}

func (runtimes *persistentMalformedRuntimes) List(context.Context) (startup.RuntimeInventorySnapshot, error) {
	return startup.RuntimeInventorySnapshot{Malformed: []startup.MalformedManagedRuntimeProcess{runtimes.malformed}}, nil
}

func (*persistentMalformedRuntimes) EnsureAbsent(context.Context, store.AgentTurnRuntimeIdentity) error {
	return nil
}

func (*persistentMalformedRuntimes) EnsureMalformedAbsent(context.Context, startup.MalformedManagedRuntimeProcess) error {
	return nil
}
