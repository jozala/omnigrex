package microsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

type qualificationPlatformFake struct {
	*fakePlatform
	err      error
	observed bool
}

func (driver *qualificationPlatformFake) ExecuteQualification(_ context.Context, _ metadata, _ []string, output *environment.QualificationOutput) error {
	_, _ = output.Write([]byte("fixture-secret\nPi resource check failed: owned checksum runner.mjs\n"))
	if driver.observed {
		output.ObserveNonzeroExit()
	}
	return driver.err
}
func TestQualificationBindingAndFailureEvidenceSurviveCleanup(t *testing.T) {
	for _, cause := range []error{errors.New("guest setup failed (observed exit 1)"), execution.ErrUnknownEffect, context.Canceled, context.DeadlineExceeded} {
		for _, cleanupFails := range []bool{false, true} {
			backend, base, _ := testBackend(t)
			observed := strings.Contains(cause.Error(), "observed exit")
			driver := &qualificationPlatformFake{fakePlatform: base, err: cause, observed: observed}
			backend.platform = driver
			cleanupErr := errors.New("native release failure")
			if cleanupFails {
				driver.removeErr = cleanupErr
			}
			var inventory piresources.ResourceContract
			_ = json.Unmarshal(piresources.CandidateContract("amd64"), &inventory)
			inventory.NodeVersion = "v24.13.0"
			resources, _ := json.Marshal(inventory)
			contract := profile.Contract{Name: "pi-rpc", Image: testRequest().Image, Platform: profile.Platform{OS: "linux", Arch: "amd64"}, GoldenResourceContract: resources}
			_, err := backend.diagnosticContract(context.Background(), contract, contract.Image, 256<<20, false)
			if !errors.Is(err, cause) || errors.Is(err, cleanupErr) != cleanupFails {
				t.Fatal(err)
			}
			if !strings.Contains(err.Error(), contract.Image) || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "selected production") {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "guest-reported failed check") != observed {
				t.Fatal(err)
			}
			if driver.stops != 1 || driver.removes != 1 {
				t.Fatalf("exact helper cleanup missing: stops=%d removes=%d", driver.stops, driver.removes)
			}
		}
	}
}
func TestQualificationWithoutCaptureAndMissingRetainedInventory(t *testing.T) {
	backend, driver, _ := testBackend(t)
	contract := profile.Contract{Name: "pi-rpc", Image: testRequest().Image, Platform: profile.Platform{OS: "linux", Arch: "amd64"}}
	_, err := backend.diagnosticContract(context.Background(), contract, contract.Image, 256<<20, false)
	if err == nil || !strings.Contains(err.Error(), "missing bound Pi resource inventory") || !strings.Contains(err.Error(), "unknown stage") || driver.removes != 1 {
		t.Fatal(err)
	}
}
