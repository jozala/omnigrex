//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/startup"
	mobyclient "github.com/moby/moby/client"
)

func TestExecutionOrphanCleanupPreservesLegitimateProfileReservation(t *testing.T) {
	const image = "alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
		t.Fatalf("pull fixture: %v\n%s", err, output)
	}
	for index, alreadyReserved := range []bool{true, false} {
		t.Run(fmt.Sprintf("reservation_exists_%t", alreadyReserved), func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			fixture := seedAgentSession(t, pool, 994+index)
			turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "orphan-cleanup", 5*time.Minute, 1)
			if err != nil {
				t.Fatal(err)
			}
			legitimate := execution.Key{AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID, AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
			orphan := legitimate
			orphan.RuntimeProfileVersion = "orphan"
			ledger := database.ExecutionLedger(lease)
			digest := strings.Repeat("a", 64)
			if alreadyReserved {
				if _, fresh, err := ledger.ReserveExecution(ctx, legitimate, digest); err != nil || !fresh {
					t.Fatalf("initial reservation = %t, %v", fresh, err)
				}
				if err := ledger.BeginExecutionPhase(ctx, legitimate, execution.Main); err != nil {
					t.Fatal(err)
				}
			}
			liveID := startOrphanCleanupFixture(t, ctx, image, legitimate)
			orphanID := startOrphanCleanupFixture(t, ctx, image, orphan)
			// Sentinels exercise each daemon-wide inventory category. None belongs
			// to the reconciler's fixture-ID allowlist, even the one claiming the
			// same full identity as the owned orphan.
			unrelated := legitimate
			unrelated.ExecutionEpoch++
			uniqueSentinel := startOrphanCleanupFixture(t, ctx, image, unrelated)
			duplicateSentinel := startOrphanCleanupFixture(t, ctx, image, orphan)
			malformed := legitimate
			malformed.RuntimeProfileVersion = ""
			malformedSentinel := startOrphanCleanupFixture(t, ctx, image, malformed)
			if alreadyReserved {
				if err := ledger.BindExecution(ctx, legitimate, liveID); err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := dockerruntime.NewRuntimeProcessInventory()
			if err != nil {
				t.Fatal(err)
			}
			defer inventory.Close()
			cleaner, err := dockerruntime.NewExactLabelCleaner(dockerruntime.ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: database})
			if err != nil {
				t.Fatal(err)
			}
			defer cleaner.Close()
			recovery := dockerruntime.NewRecovery(inventory, cleaner)
			scopedInventory := orphanFixtureInventory{source: recovery, owned: map[execution.Ref]struct{}{liveID: {}, orphanID: {}}}
			client, err := mobyclient.New(mobyclient.FromEnv)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			sentinelStarts := make(map[string]string)
			for _, id := range []string{uniqueSentinel, duplicateSentinel, malformedSentinel} {
				sentinel, err := client.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
				if err != nil || sentinel.Container.State == nil || !sentinel.Container.State.Running {
					t.Fatalf("sentinel %s did not start: %v", id, err)
				}
				sentinelStarts[id] = sentinel.Container.State.StartedAt
			}
			reconciler, err := startup.NewReconciler(database, scopedInventory, recovery, recovery, startup.ReconcilerOptions{MaxPasses: 5, MaxRuntimeProcesses: 100, MaxRecoveries: 10})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reconciler.Reconcile(ctx)
			if err != nil || result.RemovedRuntimeIdentities != 1 {
				t.Fatalf("orphan reconciliation = %#v, %v", result, err)
			}
			if _, err := client.ContainerInspect(ctx, orphanID, mobyclient.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
				t.Fatalf("orphan is not absent: %v", err)
			}
			live, err := client.ContainerInspect(ctx, liveID, mobyclient.ContainerInspectOptions{})
			if err != nil || live.Container.State == nil || !live.Container.State.Running {
				t.Fatalf("legitimate process was changed: %v", err)
			}
			for _, id := range []string{uniqueSentinel, duplicateSentinel, malformedSentinel} {
				sentinel, err := client.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
				if err != nil || sentinel.Container.State == nil || !sentinel.Container.State.Running || sentinel.Container.State.StartedAt != sentinelStarts[id] || sentinel.Container.RestartCount != 0 {
					t.Fatalf("unrelated managed sentinel %s was changed: %v", id, err)
				}
			}
			if err := database.ValidateTurnFence(ctx, lease); err != nil {
				t.Fatalf("live authority changed: %v", err)
			}
			reservation, fresh, err := ledger.ReserveExecution(ctx, legitimate, digest)
			if err != nil || fresh == alreadyReserved || reservation.Revoked || reservation.Key != legitimate {
				t.Fatalf("legitimate reservation after orphan cleanup = %#v, fresh %t, %v", reservation, fresh, err)
			}
			if alreadyReserved && (reservation.Resource != liveID || reservation.Phase != execution.Main) {
				t.Fatalf("legitimate binding changed: %#v", reservation)
			}
			if !alreadyReserved {
				if err := ledger.BeginExecutionPhase(ctx, legitimate, execution.Main); err != nil {
					t.Fatalf("orphan cleanup poisoned first dispatch: %v", err)
				}
			}
		})
	}
}

// Only immutable fixture IDs may reach the reconciler. Filtering applies before
// any cleanup policy and includes every snapshot category. A mixed duplicate
// group is normalized to a unique process when only one member is test-owned.
type orphanFixtureInventory struct {
	source startup.RuntimeInventory
	owned  map[execution.Ref]struct{}
}

func (inventory orphanFixtureInventory) List(ctx context.Context) (execution.Snapshot, error) {
	snapshot, err := inventory.source.List(ctx)
	if err != nil {
		return execution.Snapshot{}, err
	}
	var scoped execution.Snapshot
	for _, process := range snapshot.Processes {
		if _, ok := inventory.owned[process.Resource]; ok {
			scoped.Processes = append(scoped.Processes, process)
		}
	}
	for _, duplicate := range snapshot.Duplicates {
		var resources []execution.Ref
		for _, resource := range duplicate.Resources {
			if _, ok := inventory.owned[resource]; ok {
				resources = append(resources, resource)
			}
		}
		if len(resources) == 1 {
			scoped.Processes = append(scoped.Processes, execution.ManagedProcess{Identity: duplicate.Identity, Resource: resources[0]})
		} else if len(resources) > 1 {
			scoped.Duplicates = append(scoped.Duplicates, execution.DuplicateProcesses{Identity: duplicate.Identity, Resources: resources})
		}
	}
	for _, malformed := range snapshot.Malformed {
		if _, ok := inventory.owned[malformed.Resource]; ok {
			scoped.Malformed = append(scoped.Malformed, malformed)
		}
	}
	return scoped, nil
}

func TestExecutionOrphanInventoryScopesEveryCategory(t *testing.T) {
	identity := execution.Key{RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
	source := execution.Snapshot{
		Processes: []execution.ManagedProcess{{Identity: identity, Resource: "owned-unique"}, {Identity: identity, Resource: "unrelated-unique"}},
		Duplicates: []execution.DuplicateProcesses{
			{Identity: identity, Resources: []string{"unrelated-mixed", "owned-mixed"}},
			{Identity: identity, Resources: []string{"owned-a", "owned-b"}},
			{Identity: identity, Resources: []string{"unrelated-a", "unrelated-b"}},
		},
		Malformed: []execution.MalformedProcess{{Resource: "owned-malformed", Reason: "invalid_identity"}, {Resource: "unrelated-malformed", Reason: "invalid_identity"}},
	}
	inventory := orphanFixtureInventory{source: orphanInventorySnapshot(source), owned: map[string]struct{}{"owned-unique": {}, "owned-mixed": {}, "owned-a": {}, "owned-b": {}, "owned-malformed": {}}}
	got, err := inventory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := execution.Snapshot{
		Processes:  []execution.ManagedProcess{{Identity: identity, Resource: "owned-unique"}, {Identity: identity, Resource: "owned-mixed"}},
		Duplicates: []execution.DuplicateProcesses{{Identity: identity, Resources: []string{"owned-a", "owned-b"}}},
		Malformed:  []execution.MalformedProcess{{Resource: "owned-malformed", Reason: "invalid_identity"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture-scoped inventory = %#v, want %#v", got, want)
	}
}

type orphanInventorySnapshot execution.Snapshot

func (snapshot orphanInventorySnapshot) List(context.Context) (execution.Snapshot, error) {
	return execution.Snapshot(snapshot), nil
}

func startOrphanCleanupFixture(t *testing.T, ctx context.Context, image string, key execution.Key) string {
	t.Helper()
	args := []string{"run", "--detach", "--network", "none", "--user", "10001:10001", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	for _, label := range []string{
		dockerruntime.RuntimeProcessMarkerLabel + "=" + dockerruntime.RuntimeProcessMarkerValue,
		dockerruntime.RuntimeProcessAssignmentLabel + "=" + key.AssignmentID,
		dockerruntime.RuntimeProcessSessionLabel + "=" + key.AgentSessionID,
		dockerruntime.RuntimeProcessTurnLabel + "=" + key.AgentTurnID,
		dockerruntime.RuntimeProcessEpochLabel + "=" + strconv.FormatInt(key.ExecutionEpoch, 10),
		dockerruntime.RuntimeProfileIdentityLabel + "=" + key.RuntimeProfileName + "/" + key.RuntimeProfileVersion,
	} {
		args = append(args, "--label", label)
	}
	args = append(args, image, "/bin/sh", "-c", "sleep 300")
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("start fixture: %v\n%s", err, output)
	}
	id := strings.TrimSpace(string(output))
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(cleanupCtx, "docker", "rm", "--force", id).CombinedOutput(); err != nil && !strings.Contains(strings.ToLower(string(output)), "no such container") {
			t.Errorf("remove fixture: %v\n%s", err, output)
		}
	})
	return id
}
