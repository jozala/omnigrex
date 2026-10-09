package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func TestCreateRejectsChangedEphemeralInputsWithinEngine(t *testing.T) {
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	request.Environment = []string{`OPENCODE_AUTH_CONTENT={"key":"original"}`}
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	request.Environment[0] = `OPENCODE_AUTH_CONTENT={"key":"replacement"}`
	if _, err := engine.Create(context.Background(), request, io.Discard); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("changed credentials = %v", err)
	}
	if driver.creates != 0 {
		t.Fatal("conflicting inputs provisioned an environment")
	}
}

func TestPreparedExecutionRejectsDifferentRegistrationOnRetry(t *testing.T) {
	backend, _, ledger := testBackend(t)
	request := testRequest()
	request.Environment = []string{`OPENCODE_CONFIG_CONTENT={"mcp":{"gateway":{"headers":{"Authorization":"Bearer registration-B"}}}}`}
	first, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Create(context.Background(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	request.Environment[0] = `OPENCODE_CONFIG_CONTENT={"mcp":{"gateway":{"headers":{"Authorization":"Bearer registration-C"}}}}`
	retry, err := backend.New(request, ledger)
	if err == nil {
		_, err = retry.Create(context.Background(), request, io.Discard)
	}
	if !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("different registration retry = %v", err)
	}
}

func TestInputOwnershipSurvivesJournalWithoutPersistingSecrets(t *testing.T) {
	backend, _, ledger := testBackend(t)
	request := testRequest()
	request.Environment = []string{`OPENCODE_AUTH_CONTENT={"key":"provider-owned-input"}`, `OPENCODE_CONFIG_CONTENT={"mcp":{"gateway":{"headers":{"Authorization":"Bearer exact-registration"}}}}`}
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(context.Background(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A new engine in the same registration owner can recover pre-start attachment.
	retry, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retry.Create(context.Background(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(backend.options.Home, "omnigrex-operations", nameFor(request.Key)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "provider-owned-input") || strings.Contains(string(content), "exact-registration") {
		t.Fatal("journal retained credential material")
	}
	// A new control-plane instance has no registration ownership, even if it
	// happens to receive identical credentials. It must clean up, not adopt.
	restarted := &Backend{options: backend.options, platform: backend.platform, ledger: ledger, inputs: newInputBindings()}
	replacement, err := restarted.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Create(context.Background(), request, io.Discard); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("registration adoption after restart = %v", err)
	}
	if err := restarted.EnsureKeyAbsent(context.Background(), request.Key); err != nil {
		t.Fatal(err)
	}
}

func TestMissingExitEvidenceDoesNotBlockRemovalReconciliation(t *testing.T) {
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(context.Background(), request, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := backend.EnsureKeyStopped(context.Background(), request.Key); err != nil {
		t.Fatal(err)
	}
	delete(driver.resources, nameFor(request.Key))
	observation, err := backend.ObserveKeyExit(context.Background(), request.Key)
	if err != nil || observation.Exited || observation.NeverStarted || observation.OOMKilled {
		t.Fatalf("missing evidence = %+v, %v", observation, err)
	}
	if err := backend.EnsureKeyStopped(context.Background(), request.Key); err != nil {
		t.Fatal(err)
	}
	if err := backend.EnsureKeyAbsent(context.Background(), request.Key); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedDiagnosticExpiryIsReturnedAndCanBeCollected(t *testing.T) {
	backend, driver, _ := testBackend(t)
	meta := metadata{ID: "bad-expiry-id", Name: "diagnostic", Status: "Running", Labels: map[string]string{namespaceLabel: "tests", diagnosticLabel: "true", expiryLabel: "invalid"}}
	driver.resources[meta.Name] = meta
	snapshot, err := backend.List(context.Background())
	if err != nil || len(snapshot.Malformed) != 1 || len(snapshot.Processes) != 0 {
		t.Fatalf("malformed diagnostic inventory = %+v, %v", snapshot, err)
	}
	if err := backend.EnsureMalformedAbsent(context.Background(), snapshot.Malformed[0]); err != nil {
		t.Fatal(err)
	}
	if len(driver.resources) != 0 {
		t.Fatal("malformed diagnostic was not removed")
	}
	// A diagnostic carrying a valid Turn key is contradictory ownership, not a
	// valid Turn that may bypass malformed cleanup.
	key, _ := json.Marshal(testRequest().Key)
	meta.Labels[identityLabel], meta.Labels[digestLabel] = string(key), strings.Repeat("a", 64)
	driver.resources[meta.Name] = meta
	snapshot, err = backend.List(context.Background())
	if err != nil || len(snapshot.Malformed) != 1 {
		t.Fatalf("contradictory diagnostic inventory = %+v, %v", snapshot, err)
	}
	if err := backend.EnsureMalformedAbsent(context.Background(), snapshot.Malformed[0]); err != nil {
		t.Fatal(err)
	}
}

func TestPreDispatchJournalFailureDoesNotAdmitNativeCreation(t *testing.T) {
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(backend.options.Home, "omnigrex-operations", nameFor(request.Key)+".json")
	// Deny atomic publication without OS-user-dependent permission behavior.
	ledger.reserveHook = func() {
		if err := os.Mkdir(journal, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Create(context.Background(), request, io.Discard); err == nil {
		t.Fatal("journal failure ignored")
	}
	if driver.creates != 0 || ledger.records[request.Key].Phase != execution.Reserved {
		t.Fatal("native dispatch was admitted before journal preparation finished")
	}
	ledger.reserveHook = nil
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.EnsureKeyAbsent(context.Background(), request.Key); err != nil {
		t.Fatalf("known pre-dispatch cleanup failed: %v", err)
	}
}

func TestAmbiguousPhaseAdmissionWithoutDispatchIsSettled(t *testing.T) {
	backend, driver, ledger := testBackend(t)
	request := testRequest()
	ledger.beginErr = errors.New("phase transaction acknowledgement lost")
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Create(context.Background(), request, io.Discard); err == nil {
		t.Fatal("admission failure ignored")
	}
	reservation := ledger.records[request.Key]
	if driver.creates != 0 || !reservation.CreationSettled || reservation.Resource != "" {
		t.Fatal("proven no-dispatch failure retained an unresolved native create")
	}
	if err := backend.EnsureKeyAbsent(context.Background(), request.Key); err != nil {
		t.Fatalf("settled no-dispatch cleanup: %v", err)
	}
}

func TestLostJournalMustNotInventNeverStartedOrAuthorizeDisposal(t *testing.T) {
	for _, state := range []string{"Crashed", "Running"} {
		t.Run(state, func(t *testing.T) {
			backend, driver, ledger := testBackend(t)
			request := testRequest()
			engine, err := backend.New(request, ledger)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Create(context.Background(), request, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := backend.locked(context.Background(), request.Key, func(record *operation) error { record.Started = true; return nil }); err != nil {
				t.Fatal(err)
			}
			actual := driver.resources[nameFor(request.Key)]
			actual.Status = state
			driver.resources[actual.Name] = actual
			journal := filepath.Join(backend.options.Home, "omnigrex-operations", nameFor(request.Key)+".json")
			if err := os.Remove(journal); err != nil {
				t.Fatal(err)
			}
			restarted := &Backend{options: backend.options, platform: driver, ledger: ledger, inputs: newInputBindings()}
			if err := restarted.EnsureKeyStopped(context.Background(), request.Key); err != nil {
				t.Fatal(err)
			}
			observed, err := restarted.ObserveKeyExit(context.Background(), request.Key)
			if err != nil || observed.NeverStarted || observed.OOMKilled || observed.ExitCodeKnown {
				t.Fatalf("lost journal invented launch evidence: %+v,%v", observed, err)
			}
			if err := restarted.EnsureKeyAbsent(context.Background(), request.Key); !errors.Is(err, execution.ErrUnknownEffect) {
				t.Fatalf("missing launch history authorized disposal: %v", err)
			}
			if len(driver.resources) != 1 {
				t.Fatal("unknown-history environment was removed")
			}
			if err := restarted.locked(context.Background(), request.Key, func(record *operation) error {
				if record.LaunchHistoryKnown {
					t.Fatal("cleanup invented launch history")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			replacement, err := restarted.New(request, ledger)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := replacement.Create(context.Background(), request, io.Discard); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("lost journal authorized adoption: %v", err)
			}
		})
	}
}
