package profile_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestHistoricalPiPIDBudgetPreservesFrozenIdentity(t *testing.T) {
	content, err := os.ReadFile("testdata/pi-v2-pids-128.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract profile.Contract
	if err := json.Unmarshal(content, &contract); err != nil {
		t.Fatal(err)
	}
	legacy, err := profile.New(contract)
	if err != nil {
		t.Fatal(err)
	}
	const frozenHash = "f624eaf45b6ec75f2066c37170debfcc3ab11f2f08278196d484acbce1b8297f"
	if legacy.ContentSHA256() != frozenHash {
		t.Fatalf("historical PID contract hash = %s; want %s", legacy.ContentSHA256(), frozenHash)
	}
	// JSONB-style key reordering must not substitute the new PID default.
	var object map[string]any
	_ = json.Unmarshal(content, &object)
	reordered, _ := json.Marshal(object)
	resolved, err := profile.DecodeContract(reordered, legacy.Binding())
	if err != nil || resolved.Binding() != legacy.Binding() || resolved.Contract().PIDsLimit != 128 {
		t.Fatalf("historical PID budget changed on restore: %+v, %v", resolved.Binding(), err)
	}
}

func TestPiPIDBudgetsRemainExactThroughResolutionAndLaunch(t *testing.T) {
	current, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if current.Contract().PIDsLimit != 512 {
		t.Fatalf("new Participant PID budget = %d; want 512", current.Contract().PIDsLimit)
	}
	oldContract := current.Contract()
	oldContract.PIDsLimit = 128
	legacy, err := profile.New(oldContract)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Binding() == current.Binding() {
		t.Fatal("PID budget absent from immutable identity")
	}
	catalog, err := profile.NewCatalog([]profile.Profile{current}, []profile.Profile{legacy})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := catalog.Resolve(profile.PiRuntimeName, profile.PiRuntimeVersion)
	if err != nil || selected.Binding() != current.Binding() {
		t.Fatalf("current selection = %+v, %v", selected.Binding(), err)
	}
	for _, original := range []profile.Profile{current, legacy} {
		resolved, err := catalog.ResolveBinding(original.Binding())
		if err != nil || resolved.Binding() != original.Binding() {
			t.Fatalf("exact PID binding = %+v, %v", resolved.Binding(), err)
		}
		request, err := (pi.TurnAdapter{}).BuildProcess(resolved, execution.Key{RuntimeProfileName: profile.PiRuntimeName, RuntimeProfileVersion: profile.PiRuntimeVersion}, "", 1<<30, nil)
		if err != nil || request.PIDsLimit != original.Contract().PIDsLimit {
			t.Fatalf("launch PID budget = %d, %v; want %d", request.PIDsLimit, err, original.Contract().PIDsLimit)
		}
	}
	encoded, _ := json.Marshal(current.Contract())
	if _, err := profile.DecodeContract(encoded, legacy.Binding()); !errors.Is(err, profile.ErrConflict) {
		t.Fatalf("cross-budget binding accepted: %v", err)
	}
	for _, budget := range []int64{-1, 0, 129, 256, 513} {
		contract := current.Contract()
		contract.PIDsLimit = budget
		if _, err := profile.New(contract); err == nil {
			t.Fatalf("unqualified PID budget accepted: %d", budget)
		}
	}
}

func TestPIDBudgetCompatibilityDoesNotAdmitPreBaselineContracts(t *testing.T) {
	current, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int64{128, 512} {
		contract := current.Contract()
		contract.Version, contract.PIDsLimit = "v1", budget
		if _, err := profile.New(contract); !errors.Is(err, profile.ErrInvalid) {
			t.Fatalf("pre-baseline contract accepted for PID budget %d: %v", budget, err)
		}
	}
}
