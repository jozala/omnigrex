//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jozala/omnigrex/internal/agentturn"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestNeverStartedRuntimeRecoveryCompletesWithRealDocker(t *testing.T) {
	if os.Getenv("DOCKER_HOST") != "" {
		t.Fatal("never-started runtime fixtures require local Docker with DOCKER_HOST unset")
	}
	for index, test := range []struct {
		name            string
		failedStart     bool
		lostCreationAck bool
		retainedSession bool
	}{
		{name: "crash after create before start"},
		{name: "native start fails and remains created", failedStart: true},
		{name: "lost creation acknowledgement", lostCreationAck: true},
		{name: "already bound retained developer Session", retainedSession: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			postgres := startPostgres(t)
			password := filepath.Join(t.TempDir(), "database-password")
			if err := os.WriteFile(password, []byte(postgresPassword), 0o600); err != nil {
				t.Fatal(err)
			}
			open := func() *store.Store {
				t.Helper()
				database, err := store.Open(ctx, postgres.databaseURL(false), password, builtinStoreConfig(t))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(database.Close)
				return database
			}
			database := open()
			pool := openPool(t, postgres.databaseURL(true))
			triggerPreparationWorkflow(t, database, ctx,
				fmt.Sprintf("7d000000-0000-4000-8000-%012d", index+1), fmt.Sprintf("7d100000-0000-4000-8000-%012d", index+1))
			prepared := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "never-started-profile", "openai/never-started")
			if test.retainedSession {
				prior := acquireAndBindTurn(t, database, pool, ctx, prepared, "retained-never-started-session")
				if err := phaseNineMainExecution(ctx, database, prior); err != nil {
					t.Fatal(err)
				}
				if err := database.OpenMutationAdmission(ctx, prior); err != nil {
					t.Fatal(err)
				}
				if err := database.CloseMutationAdmission(ctx, prior); err != nil {
					t.Fatal(err)
				}
				if settled, err := database.SettleAgentTurn(ctx, prior, failedSettlementObservation("prior runtime failed")); err != nil || settled.Reason != workflow.ReasonInfrastructureRetry {
					t.Fatalf("prior settlement = (%#v, %v)", settled, err)
				}
				current := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "never-started-profile", "openai/never-started")
				if current.Session.ID != prepared.Session.ID || current.Session.RuntimeSessionID != prepared.Session.ID || current.Turn.RetryOfTurnID != prepared.Turn.ID {
					t.Fatal("fixture did not retain the already bound Session")
				}
				prepared = current
			}
			lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, prepared.Turn), prepared.Turn.ControlRevision, "never-started-owner", time.Minute, 1)
			if err != nil {
				t.Fatal(err)
			}
			key := execution.Key{AssignmentID: lease.AgentAssignmentID, AgentSessionID: lease.AgentSessionID,
				AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch,
				RuntimeProfileName: prepared.Participant.RuntimeProfileName, RuntimeProfileVersion: prepared.Participant.RuntimeProfileVersion}
			digest := strings.Repeat("a", 64)
			ledger := database.ExecutionLedger(lease)
			if _, _, err := ledger.ReserveExecution(ctx, key, digest); err != nil {
				t.Fatal(err)
			}
			if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
				t.Fatal(err)
			}
			client, err := mobyclient.New(mobyclient.FromEnv)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			image, err := client.ImageInspect(ctx, "omnigrex/pi:1.0.4")
			if err != nil {
				t.Fatalf("inspect built Pi image: %v", err)
			}
			created, err := client.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
				Name: "omnigrex-turn-" + key.AgentTurnID + "-epoch-" + strconv.FormatInt(key.ExecutionEpoch, 10),
				Config: &container.Config{Image: image.ID, Entrypoint: []string{"/nonexistent-omnigrex-never-started"}, Labels: map[string]string{
					dockerruntime.RuntimeProcessMarkerLabel:     dockerruntime.RuntimeProcessMarkerValue,
					dockerruntime.RuntimeProcessAssignmentLabel: key.AssignmentID,
					dockerruntime.RuntimeProcessSessionLabel:    key.AgentSessionID,
					dockerruntime.RuntimeProcessTurnLabel:       key.AgentTurnID,
					dockerruntime.RuntimeProcessEpochLabel:      strconv.FormatInt(key.ExecutionEpoch, 10),
					dockerruntime.RuntimeProfileIdentityLabel:   key.RuntimeProfileName + "/" + key.RuntimeProfileVersion,
					"io.omnigrex.execution-specification":       digest,
				}},
				HostConfig: &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cleanupClient, err := mobyclient.New(mobyclient.FromEnv)
				if err != nil {
					t.Error(err)
					return
				}
				defer cleanupClient.Close()
				if _, err := cleanupClient.ContainerRemove(cleanup, created.ID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
					t.Errorf("remove fixture: %v", err)
				}
			})
			if !test.lostCreationAck {
				if err := ledger.BindExecution(ctx, key, created.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.failedStart {
				if _, err := client.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err == nil {
					t.Fatal("nonexistent entrypoint unexpectedly started")
				}
			}
			assertCreated := func() {
				t.Helper()
				actual, err := client.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
				if err != nil || actual.Container.State == nil {
					t.Fatalf("inspect created fixture: %v", err)
				}
				state := actual.Container.State
				started, err := time.Parse(time.RFC3339Nano, state.StartedAt)
				if err != nil || !started.IsZero() || state.Status != container.StateCreated || state.Running || state.Pid != 0 || state.Paused || state.Restarting || state.Dead || state.OOMKilled {
					t.Fatalf("fixture is not authoritative never-started: %#v, %v", state, err)
				}
			}
			assertCreated()
			var resource string
			var proof bool
			if err := pool.QueryRow(ctx, `SELECT COALESCE(resource_ref, ''), preparation_only_cleanup FROM execution_reservations WHERE agent_turn_id = $1 AND execution_epoch = $2`, lease.ID, lease.ExecutionEpoch).Scan(&resource, &proof); err != nil || proof || (resource == "") != test.lostCreationAck {
				t.Fatalf("pre-recovery reservation = (%q, %t, %v)", resource, proof, err)
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			cleaner, err := dockerruntime.NewExactLabelCleaner(dockerruntime.ExactLabelCleanerOptions{StopTimeout: time.Second, Ledger: database})
			if err != nil {
				t.Fatal(err)
			}
			defer cleaner.Close()
			captures := 0
			worker, err := agentturn.NewStopWorker(database, cleaner, phaseNineWorkspaceDiscarder{captureErr: execution.ErrUnknownEffect, captures: &captures}, agentturn.StopWorkerConfig{
				ClaimOwner: "never-started-stop", LeaseDuration: 5 * time.Second, HeartbeatInterval: 100 * time.Millisecond,
				IdlePollInterval: time.Millisecond, CleanupRetryInterval: time.Millisecond,
				TerminalCaptureRequired: func(ctx context.Context, recovery store.JobLease) (bool, error) {
					assertCreated() // Native stop preserves created state and zero StartedAt.
					if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, store.ErrAgentTurnFenceLost) {
						t.Fatalf("old owner dispatch after recovery = %v", err)
					}
					if err := database.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, execution.ErrConflict) {
						t.Fatalf("backend dispatch after recovery = %v", err)
					}
					return database.AgentTurnTerminalCaptureRequired(ctx, recovery)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if processed, err := worker.ProcessNext(ctx); err != nil || !processed || captures != 0 {
				t.Fatalf("never-started recovery = (processed %t, error %v, captures %d), want completed without capture", processed, err, captures)
			}
			if _, err := client.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); !errdefs.IsNotFound(err) {
				t.Fatalf("late native start of immutable resource = %v, want NotFound", err)
			}
			database.Close()
			database = open()
			reservation, found, err := database.RevokeExecution(ctx, key)
			if err != nil || !found || reservation.Resource != created.ID || !reservation.Revoked || !reservation.CreationSettled {
				t.Fatalf("reopened reservation = (%#v, %t, %v)", reservation, found, err)
			}
			var succeeded, stopped, oom bool
			if err := pool.QueryRow(ctx, `SELECT job.status = 'SUCCEEDED', turn.runtime_stopped_at IS NOT NULL,
    reservation.preparation_only_cleanup, turn.runtime_oom_observed_at IS NOT NULL
FROM agent_turns AS turn JOIN jobs AS job ON job.id = turn.stop_runtime_job_id
JOIN execution_reservations AS reservation ON reservation.agent_turn_id = turn.id AND reservation.execution_epoch = turn.execution_epoch
WHERE turn.id = $1 AND turn.execution_epoch = $2`, lease.ID, lease.ExecutionEpoch).Scan(&succeeded, &stopped, &proof, &oom); err != nil || !succeeded || !stopped || !proof || oom {
				t.Fatalf("durable stop/proof = (job %t, stopped %t, proof %t, OOM %t, error %v)", succeeded, stopped, proof, oom, err)
			}
		})
	}
}

func TestRecoveredNeverStartedProofRejectsConflictingEvidence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for index, test := range []string{
		"empty resource", "wrong resource", "whitespace resource", "oversized resource",
		"reserved phase", "setup phase", "nonrevoked reservation", "unsettled creation", "missing resource",
		"wrong assignment", "wrong Session", "wrong Turn", "wrong epoch", "wrong profile name", "wrong profile version",
		"human prompt", "open admission", "prompt checkpoint", "invocation counter", "tool invocation",
		"stale token", "stale epoch", "stale attempt",
		"monotonic proof and later activity", "monotonic proof and later admission", "monotonic proof and later invocation counter",
		"expired recovery lease",
	} {
		t.Run(test, func(t *testing.T) {
			monotonic := strings.HasPrefix(test, "monotonic proof")
			fixture := seedAgentSession(t, pool, 1800+index)
			turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "never-started-proof-owner", time.Minute, 1)
			if err != nil {
				t.Fatal(err)
			}
			key := execution.Key{AssignmentID: lease.AgentAssignmentID, AgentSessionID: lease.AgentSessionID,
				AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
			ledger := database.ExecutionLedger(lease)
			if _, _, err := ledger.ReserveExecution(ctx, key, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
				t.Fatal(err)
			}
			if err := ledger.BindExecution(ctx, key, "never-started-main"); err != nil {
				t.Fatal(err)
			}
			if test == "tool invocation" {
				if err := database.OpenMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				if _, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "comment_on_issue", OperationID: "never-started-conflict", Request: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
				// Isolate the durable invocation check from the counter check.
				if _, err := pool.Exec(ctx, `UPDATE agent_turns SET next_invocation_number = 1 WHERE id = $1`, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
			if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
				t.Fatal(err)
			}
			recovery, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "never-started-proof-stop", time.Minute)
			if err != nil || recovery == nil || recovery.AgentTurnID != lease.ID {
				t.Fatalf("claim recovery = (%#v, %v)", recovery, err)
			}
			if _, _, err := database.RevokeExecution(ctx, key); err != nil {
				t.Fatal(err)
			}
			if monotonic {
				for range 2 {
					if err := database.RecordRecoveredRuntimeNeverStarted(ctx, *recovery, "never-started-main"); err != nil {
						t.Fatalf("record idempotent proof for already bound Session: %v", err)
					}
				}
				if required, err := database.AgentTurnTerminalCaptureRequired(ctx, *recovery); err != nil || required {
					t.Fatalf("preparation-only capture requirement = (%t, %v)", required, err)
				}
				if _, err := pool.Exec(ctx, `UPDATE execution_reservations SET preparation_only_cleanup = false WHERE agent_turn_id = $1 AND execution_epoch = $2`, key.AgentTurnID, key.ExecutionEpoch); err == nil {
					t.Fatal("cleanup proof was not monotonic")
				}
			}
			resource := "never-started-main"
			var update string
			args := []any{lease.ID}
			want := execution.ErrConflict
			switch test {
			case "empty resource":
				resource = ""
			case "wrong resource":
				resource = "another-main"
			case "whitespace resource":
				resource = " never-started-main"
			case "oversized resource":
				resource = strings.Repeat("x", 129)
			case "reserved phase":
				update = `UPDATE execution_reservations SET dispatch_phase = 'reserved', resource_ref = NULL WHERE agent_turn_id = $1`
			case "setup phase":
				update = `UPDATE execution_reservations SET dispatch_phase = 'setup', resource_ref = NULL WHERE agent_turn_id = $1`
			case "nonrevoked reservation":
				update = `UPDATE execution_reservations SET revoked = false WHERE agent_turn_id = $1`
			case "unsettled creation":
				update = `UPDATE execution_reservations SET creation_settled = false WHERE agent_turn_id = $1`
			case "missing resource":
				update = `UPDATE execution_reservations SET resource_ref = NULL WHERE agent_turn_id = $1`
			case "wrong assignment", "wrong Session", "wrong Turn", "wrong epoch", "wrong profile name", "wrong profile version":
				wrong := key
				switch test {
				case "wrong assignment":
					wrong.AssignmentID = "10000000-0000-4000-8000-000000000001"
				case "wrong Session":
					wrong.AgentSessionID = "20000000-0000-4000-8000-000000000001"
				case "wrong Turn":
					wrong.AgentTurnID = "30000000-0000-4000-8000-000000000001"
				case "wrong epoch":
					wrong.ExecutionEpoch++
				case "wrong profile name":
					wrong.RuntimeProfileName = "other-runtime"
				case "wrong profile version":
					wrong.RuntimeProfileVersion = "other-version"
				}
				identity, err := json.Marshal(wrong)
				if err != nil {
					t.Fatal(err)
				}
				update = `UPDATE execution_reservations SET identity = $2::jsonb WHERE agent_turn_id = $1`
				args = append(args, identity)
			case "human prompt":
				update = `UPDATE agent_sessions SET control_owner = 'HUMAN',
human_prompt_token = gen_random_uuid(), human_prompt_leased_at = clock_timestamp(),
human_prompt_heartbeat_at = clock_timestamp(), human_prompt_lease_expires_at = clock_timestamp() + interval '1 minute'
WHERE id = $1`
				args[0] = lease.AgentSessionID
			case "open admission", "monotonic proof and later admission":
				update = `UPDATE agent_turns SET mutation_admission_open = true WHERE id = $1`
			case "prompt checkpoint", "monotonic proof and later activity":
				update = `UPDATE agent_turns SET prompt_stop_reason = 'completed', prompt_recorded_at = clock_timestamp() WHERE id = $1`
			case "invocation counter", "monotonic proof and later invocation counter":
				update = `UPDATE agent_turns SET next_invocation_number = 2 WHERE id = $1`
			case "stale token":
				recovery.LeaseToken = "17000000-0000-4000-8000-000000000001"
				want = store.ErrAgentTurnRecoveryFenceLost
			case "stale epoch":
				recovery.ExecutionEpoch++
				want = store.ErrAgentTurnRecoveryFenceLost
			case "stale attempt":
				recovery.Attempt++
				want = store.ErrAgentTurnRecoveryFenceLost
			case "expired recovery lease":
				update = `UPDATE jobs SET lease_expires_at = clock_timestamp() - interval '1 microsecond' WHERE id = $1`
				args[0] = recovery.ID
				want = store.ErrAgentTurnRecoveryFenceLost
			}
			if update != "" {
				if _, err := pool.Exec(ctx, update, args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.RecordRecoveredRuntimeNeverStarted(ctx, *recovery, resource); !errors.Is(err, want) {
				t.Fatalf("conflicting proof = %v, want %v", err, want)
			}
			var proof bool
			if err := pool.QueryRow(ctx, `SELECT preparation_only_cleanup FROM execution_reservations WHERE agent_turn_id = $1 AND execution_epoch = $2`, key.AgentTurnID, key.ExecutionEpoch).Scan(&proof); err != nil || proof != monotonic {
				t.Fatalf("denied proof = (%t, %v)", proof, err)
			}
			if monotonic {
				if required, err := database.AgentTurnTerminalCaptureRequired(ctx, *recovery); err != nil || !required {
					t.Fatalf("later activity did not override capture exemption = (%t, %v)", required, err)
				}
			}
		})
	}
}

func TestRecoveredNeverStartedProofRejectsMutationReplayWithNoCurrentInvocations(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 1900)
	root, prior, _ := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "never-started-replay-root")
	spec := store.MutationSpec{ToolName: "comment_on_issue", OperationID: "never-started-ancestor", Request: json.RawMessage(`{}`)}
	mutation, err := database.ReserveMutation(ctx, prior, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, prior, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, prior, mutation.ID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, prior); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, prior, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "prior prompt failed"}); err != nil {
		t.Fatal(err)
	}
	_, lease, _ := acquireReplayGatewayTurn(t, database, pool, fixture, root, "never-started-replay-retry")
	if err := database.AcknowledgeMutationReplay(ctx, lease, mutation.ID, spec); err != nil {
		t.Fatal(err)
	}
	// Isolate replay evidence from the invocation counter and open admission.
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET next_invocation_number = 1 WHERE id = $1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: lease.AgentAssignmentID, AgentSessionID: lease.AgentSessionID,
		AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
	ledger := database.ExecutionLedger(lease)
	if _, _, err := ledger.ReserveExecution(ctx, key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindExecution(ctx, key, "never-started-replay-main"); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	recovery, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.StopStaleRuntimeJobKind, "never-started-replay-stop", time.Minute)
	if err != nil || recovery == nil || recovery.AgentTurnID != lease.ID {
		t.Fatalf("claim replay recovery = (%#v, %v)", recovery, err)
	}
	if _, _, err := database.RevokeExecution(ctx, key); err != nil {
		t.Fatal(err)
	}
	var invocations, replays int
	if err := pool.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM tool_invocations WHERE agent_turn_id = $1 AND execution_epoch = $2),
    (SELECT count(*) FROM tool_invocation_replays WHERE agent_turn_id = $1 AND execution_epoch = $2)`, lease.ID, lease.ExecutionEpoch).Scan(&invocations, &replays); err != nil || invocations != 0 || replays != 1 {
		t.Fatalf("isolated replay evidence = (invocations %d, replays %d, error %v)", invocations, replays, err)
	}
	if err := database.RecordRecoveredRuntimeNeverStarted(ctx, *recovery, "never-started-replay-main"); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("replayed activity admitted never-started proof: %v", err)
	}
	if required, err := database.AgentTurnTerminalCaptureRequired(ctx, *recovery); err != nil || !required {
		t.Fatalf("replayed activity capture requirement = (%t, %v)", required, err)
	}
}
