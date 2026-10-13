//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

func TestExecutionReservationsSurviveRestartAndFenceExactEpochs(t *testing.T) {
	postgres := startPostgres(t)
	ctx := context.Background()
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0600); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 1, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
	digest := strings.Repeat("a", 64)
	if _, fresh, err := database.ReserveExecution(ctx, key, digest); err != nil || !fresh {
		t.Fatalf("reserve = %t, %v", fresh, err)
	}
	if _, fresh, err := database.ReserveExecution(ctx, key, digest); err != nil || fresh {
		t.Fatalf("repeat reserve = %t, %v", fresh, err)
	}
	if _, _, err := database.ReserveExecution(ctx, key, strings.Repeat("b", 64)); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("changed spec = %v", err)
	}
	wrong := key
	wrong.AgentSessionID = "20000000-0000-4000-8000-000000000002"
	if _, _, err := database.RevokeExecution(ctx, wrong); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("wrong owner revocation = %v", err)
	}
	wrongProfile := key
	wrongProfile.RuntimeProfileVersion = "other"
	if _, _, err := database.RevokeExecution(ctx, wrongProfile); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("wrong Runtime Profile revocation = %v", err)
	}
	if _, _, err := database.ReserveExecution(ctx, key, digest); err != nil {
		t.Fatalf("wrong owner revoked legitimate intent: %v", err)
	}
	if err := database.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
		t.Fatal(err)
	}
	if err := database.BindExecution(ctx, key, "resource-1"); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordNoExecution(ctx, key); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("clearing a bound resource = %v", err)
	}
	if err := database.BindExecution(ctx, key, ""); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("empty binding = %v", err)
	}
	if err := database.BindExecution(ctx, key, "different-resource"); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("rebind = %v", err)
	}
	database.Close()
	database, err = store.Open(ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	reservation, found, err := database.RevokeExecution(ctx, key)
	if err != nil || !found || reservation.Resource != "resource-1" || !reservation.Revoked {
		t.Fatalf("recovered reservation = %#v, %t, %v", reservation, found, err)
	}
	if _, _, err := database.ReserveExecution(ctx, key, digest); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("revoked creation = %v", err)
	}
	key.ExecutionEpoch++
	if _, fresh, err := database.ReserveExecution(ctx, key, digest); err != nil || !fresh {
		t.Fatalf("next epoch = %t, %v", fresh, err)
	}
	if err := database.BeginExecutionPhase(ctx, key, execution.Main); err != nil {
		t.Fatal(err)
	}
	reservation, found, err = database.RevokeExecution(ctx, key)
	if err != nil || !found || reservation.Resource != "" {
		t.Fatalf("ambiguous reservation = %#v, %t, %v", reservation, found, err)
	}
	if reservation.CreationSettled {
		t.Fatal("unresolved creation was marked settled")
	}
	key.ExecutionEpoch++
	if _, fresh, err := database.ReserveExecution(ctx, key, digest); err != nil || !fresh {
		t.Fatal(err)
	}
	if err := database.RecordNoExecution(ctx, key); err != nil {
		t.Fatal(err)
	}
	reservation, found, err = database.RevokeExecution(ctx, key)
	if err != nil || !found || !reservation.CreationSettled || reservation.Resource != "" {
		t.Fatalf("known empty reservation = %#v, %t, %v", reservation, found, err)
	}
	if err := database.BindExecution(ctx, key, "late-foreign-resource"); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("rebinding a settled empty execution = %v", err)
	}
}

func TestExecutionRevocationFencesPreviouslyUnseenKey(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 991)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID, AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
	if _, _, err := databases[0].RevokeExecution(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := databases[0].ReserveExecution(ctx, key, strings.Repeat("a", 64)); !errors.Is(err, execution.ErrConflict) || fresh {
		t.Fatalf("reservation after unseen-key revocation = fresh %t, %v", fresh, err)
	}
}

func TestExecutionFirstReservationRacingRevocationCannotDispatch(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for sequence := 1; sequence <= 20; sequence++ {
		key := execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: fmt.Sprintf("30000000-0000-4000-8000-%012d", sequence), ExecutionEpoch: 1, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, _, err := databases[0].ReserveExecution(ctx, key, strings.Repeat("a", 64))
			results <- err
		}()
		go func() { <-start; _, _, err := databases[1].RevokeExecution(ctx, key); results <- err }()
		close(start)
		for range 2 {
			if err := <-results; err != nil && !errors.Is(err, execution.ErrConflict) {
				t.Fatal(err)
			}
		}
		if err := databases[0].BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("revoked racing key can dispatch: %v", err)
		}
		if _, fresh, err := databases[0].ReserveExecution(ctx, key, strings.Repeat("a", 64)); fresh || !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("revoked racing key can re-reserve: %t, %v", fresh, err)
		}
	}
}

func TestExecutionSetupIdentityAndPhaseSurviveStoreReopen(t *testing.T) {
	postgres := startPostgres(t)
	password := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(password, []byte(postgresPassword), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false), password, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 1, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
	if _, _, err := database.ReserveExecution(ctx, key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := database.BeginExecutionPhase(ctx, key, execution.Setup); err != nil {
		t.Fatal(err)
	}
	if err := database.BindSetupResource(ctx, key, "known-setup-id"); err != nil {
		t.Fatal(err)
	}
	if err := database.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("main dispatched before setup stop: %v", err)
	}
	database.Close()
	database, err = store.Open(ctx, postgres.databaseURL(false), password, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	reservation, found, err := database.RevokeExecution(ctx, key)
	if err != nil || !found || reservation.Phase != execution.Setup || reservation.SetupResource != "known-setup-id" || reservation.SetupSettled || reservation.Resource != "" {
		t.Fatalf("reopened setup reservation = %#v, %t, %v", reservation, found, err)
	}
	if err := database.RecordSetupStopped(ctx, key, "known-setup-id"); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordNoExecution(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := database.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("main dispatched after setup recovery revoked key: %v", err)
	}
}

func TestExecutionAdmissionChecksLiveFenceBeforeEachPhase(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	fixture := seedAgentSession(t, pool, 993)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := prepareFixtureAgentTurn(t, databases[0], pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireFixtureAgentTurn(t, databases[0], pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "runtime", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: fixture.assignmentID, AgentSessionID: fixture.sessionID, AgentTurnID: turn.ID, ExecutionEpoch: turn.ExecutionEpoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
	ledger := databases[0].ExecutionLedger(lease)
	if _, _, err := ledger.ReserveExecution(ctx, key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginExecutionPhase(ctx, key, execution.Main); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale lease admitted main creation: %v", err)
	}
}
