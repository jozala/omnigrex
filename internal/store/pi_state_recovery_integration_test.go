//go:build integration

package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	mobyclient "github.com/moby/moby/client"
)

func TestPiProtectedStateRecovery(t *testing.T) {
	f := newPiWorkflowFixture(t, "")
	prepared := f.prepare(t)
	participant := prepared.Commit.Participant
	// Offline recovery must fail while the durable writer is admitted, even
	// before a physical execution exists.
	if err := f.database.WithStateRecovery(f.ctx, participant.ID, func(execution.StateRef, execution.StateMetadataSource) error { return nil }); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("live Turn allowed offline recovery: %v", err)
	}
	if processed, err := f.worker.ProcessNext(f.ctx); err != nil || !processed {
		t.Fatalf("source Pi Turn: %t %v", processed, err)
	}
	f.assertState(t, workflow.StateReviewing)
	var archive bytes.Buffer
	var bound execution.StateRef
	err := f.database.WithStateRecovery(f.ctx, participant.ID, func(ref execution.StateRef, source execution.StateMetadataSource) error {
		bound = ref
		recovery, err := composition.StateRecovery(f.ctx, f.settings, source)
		if err != nil {
			return err
		}
		return recovery.Export(f.ctx, ref, &archive)
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Format != profile.PiStateContractVersion || bound.Image != f.runtime.Binding().Image || bound.ParticipantID != participant.ID {
		t.Fatalf("exported binding changed: %+v", bound)
	}
	if err := f.backend.States.Collect(f.ctx, bound); !errors.Is(err, store.ErrAssignmentCollectionFenceLost) {
		t.Fatalf("protected Pi Session collected: %v", err)
	}
	if err := f.backend.States.Check(f.ctx); err != nil {
		t.Fatal(err)
	}
	candidate, err := profile.NewPiV1("registry.example/omnigrex/pi@sha256:"+strings.Repeat("c", 64), f.runtime.Contract().Platform)
	if err != nil {
		t.Fatal(err)
	}
	// A current candidate does not remove historical protected-image readiness.
	catalog, err := profile.NewCatalog([]profile.Profile{candidate}, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := piRecoveryImages{actual: f.backend.Images, expected: f.runtime.Binding().Image, local: os.Getenv("OMNIGREX_PI_WORKFLOW_LOCAL_IMAGE")}
	checker, err := profile.NewAvailabilityChecker(f.database, profile.Resolver{Catalog: catalog, Source: f.database}, &images)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(f.ctx); err != nil {
		t.Fatal(err)
	}
	images.unavailable = true
	if err := checker.Check(f.ctx); !errors.Is(err, profile.ErrProtectedImageUnavailable) {
		t.Fatalf("missing retained source image did not fail readiness: %v", err)
	}
	result := profile.CompatibilityResult{Source: f.runtime.Binding(), Target: candidate.Binding(), Platform: candidate.Contract().Platform, StateContractVersion: profile.PiStateContractVersion, WorkspacePath: "/workspace", QualificationSuite: profile.PiCompatibilityQualificationSuite, QualificationVersion: "1", QualifiedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), Outcome: "success", BackendFamily: bound.Backend.Family, BackendContractVersion: bound.Backend.Version}
	file := profile.CompatibilityResultsFile{SchemaVersion: profile.PiCompatibilityResultsSchemaVersion, Results: []profile.CompatibilityResult{result}}
	for range 2 {
		if err := f.database.ImportRuntimeProfileCompatibilityResults(f.ctx, file); err != nil {
			t.Fatal(err)
		}
	}
	other := result
	if bound.Backend.Family == "docker" {
		other.BackendFamily = "microsandbox"
	} else {
		other.BackendFamily = "docker"
	}
	if err := f.database.RecordRuntimeProfileCompatibilityResult(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	conflict := result
	conflict.QualifiedAt = time.Now().UTC().Add(time.Minute).Truncate(time.Second).Format(time.RFC3339)
	if err := f.database.RecordRuntimeProfileCompatibilityResult(f.ctx, conflict); !errors.Is(err, store.ErrRuntimeProfileCompatibilityResultConflict) {
		t.Fatalf("immutable qualification overwritten: %v", err)
	}
	retained, err := f.database.GetAgentParticipant(f.ctx, participant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.RuntimeImageDigest != participant.RuntimeImageDigest || retained.RuntimeProfileContentSHA256 != participant.RuntimeProfileContentSHA256 {
		t.Fatal("qualification rebound retained Participant")
	}
	wrong := bound
	wrong.Image = candidate.Binding().Image
	wrong.RuntimeProfileSHA256 = candidate.ContentSHA256()
	if err := f.backend.States.Ensure(f.ctx, wrong); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("candidate attached live source state: %v", err)
	}
	backendChange := bound.Backend
	backendChange.Family = "other"
	if err := f.database.BindExecutionBackend(f.ctx, backendChange); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("backend change reinterpreted state: %v", err)
	}
	// Relocate the complete ownership-bound archive through a fresh storage
	// adapter. The recovered ledger supplies the original immutable metadata.
	replacementRoot := t.TempDir()
	err = f.database.WithStateRecovery(f.ctx, participant.ID, func(ref execution.StateRef, source execution.StateMetadataSource) error {
		recovered, err := dirstate.New(replacementRoot, ref.Backend, source)
		if err != nil {
			return err
		}
		for range 2 {
			if err := recovered.Restore(f.ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
				return err
			}
		}
		if err := recovered.Inspect(f.ctx, ref); err != nil {
			return err
		}
		var reexport bytes.Buffer
		if err := recovered.Export(f.ctx, ref, &reexport); err != nil {
			return err
		}
		if !bytes.Equal(archive.Bytes(), reexport.Bytes()) {
			t.Fatal("relocated archive changed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	durable, err := f.database.GetAgentSessionForParticipant(f.ctx, participant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(replacementRoot, "assignment-"+participant.ID, "runtime-state", "sessions", durable.ID, "session.jsonl")); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(f.settings.RuntimeStateRoot, "assignment-"+participant.ID, "runtime-state")
	saved := statePath + ".saved"
	if err := os.Rename(statePath, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(saved, statePath); err != nil {
			t.Error(err)
		}
	})
	if err := f.backend.States.Check(f.ctx); !errors.Is(err, execution.ErrStateUnavailable) {
		t.Fatalf("lost protected Pi data considered available: %v", err)
	}
	if err := f.backend.States.Ensure(f.ctx, bound); !errors.Is(err, execution.ErrStateUnavailable) {
		t.Fatalf("lost Pi data recreated: %v", err)
	}
}

// Discovery-tag fixtures retain an exact local Docker identity instead of a
// registry publication. Native fixtures use their actual content-verified cache.
type piRecoveryImages struct {
	actual          profile.ImageAvailability
	expected, local string
	unavailable     bool
}

func (images *piRecoveryImages) Available(ctx context.Context, image string, platform profile.Platform) error {
	if image != images.expected || images.unavailable {
		return execution.ErrNotFound
	}
	if images.local == "" {
		return images.actual.Available(ctx, image, platform)
	}
	api, err := mobyclient.New(mobyclient.FromEnv, mobyclient.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer api.Close()
	// A local content ID already selects one image, so platform selection is
	// unnecessary and would require API 1.49 on otherwise supported daemons.
	inspected, err := api.ImageInspect(ctx, images.local)
	if err != nil {
		return err
	}
	if inspected.Os != platform.OS || inspected.Architecture != platform.Arch {
		return execution.ErrBindingMismatch
	}
	return nil
}
