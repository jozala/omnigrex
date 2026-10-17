//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"

	"github.com/jozala/omnigrex/internal/workflow"
)

func TestRuntimeProfileCompatibilityResultRecordIsIdempotentAndConflictDetecting(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database, ctx := databases[0], t.Context()
	source, target := compatibilityProfile(t, "a", "amd64"), compatibilityProfile(t, "b", "amd64")
	result := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform,
		time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	for range 2 {
		if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
			t.Fatal(err)
		}
	}
	file := runtimeprofile.CompatibilityResultsFile{SchemaVersion: runtimeprofile.PiCompatibilityResultsSchemaVersion, Results: []runtimeprofile.CompatibilityResult{result}}
	if err := database.ImportRuntimeProfileCompatibilityResults(ctx, file); err != nil {
		t.Fatal(err)
	}
	conflict := result
	conflict.QualifiedAt = "2026-09-06T12:00:01Z"
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, conflict); !errors.Is(err, store.ErrRuntimeProfileCompatibilityResultConflict) {
		t.Fatalf("changed evidence replaced immutable result: %v", err)
	}
	var count int
	var content []byte
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pi_runtime_profile_compatibility_results`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("result count = %d, %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT result FROM pi_runtime_profile_compatibility_results`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	var recorded runtimeprofile.CompatibilityResult
	if err := json.Unmarshal(content, &recorded); err != nil || recorded != result {
		t.Fatalf("recorded result changed: %+v %v", recorded, err)
	}
	for _, statement := range []string{`UPDATE pi_runtime_profile_compatibility_results SET result = '{}'`, `DELETE FROM pi_runtime_profile_compatibility_results`} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Fatal("immutable Pi evidence mutation succeeded")
		}
	}
}

func TestPiCompatibilityResultsAreBackendScopedAndImmutable(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database, ctx := databases[0], t.Context()
	source, target := compatibilityProfile(t, "a", "amd64"), compatibilityProfile(t, "b", "amd64")
	result := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	other := result
	other.BackendFamily = "microsandbox"
	file := runtimeprofile.CompatibilityResultsFile{SchemaVersion: runtimeprofile.PiCompatibilityResultsSchemaVersion, Results: []runtimeprofile.CompatibilityResult{result, other}}
	for range 2 {
		if err := database.ImportRuntimeProfileCompatibilityResults(ctx, file); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pi_runtime_profile_compatibility_results`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("backend qualification count: %d %v", count, err)
	}
	if err := file.ValidateDeployment("docker", 1, target); err == nil {
		t.Fatal("Microsandbox evidence accepted for a Docker installation")
	}
}

func TestPiCompatibilityImportRollsBackOnExistingEvidenceConflict(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database, ctx := databases[0], t.Context()
	source, target := compatibilityProfile(t, "a", "amd64"), compatibilityProfile(t, "b", "amd64")
	baseline := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	newResult, conflict := baseline, baseline
	newResult.BackendFamily = "microsandbox"
	conflict.QualifiedAt = "2026-10-10T12:00:01Z"
	file := runtimeprofile.CompatibilityResultsFile{SchemaVersion: runtimeprofile.PiCompatibilityResultsSchemaVersion, Results: []runtimeprofile.CompatibilityResult{newResult, conflict}}
	if err := database.ImportRuntimeProfileCompatibilityResults(ctx, file); !errors.Is(err, store.ErrRuntimeProfileCompatibilityResultConflict) {
		t.Fatalf("conflicting artifact import: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pi_runtime_profile_compatibility_results`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("conflict retained partial artifact: %d %v", count, err)
	}
}

func TestPiQualificationCannotRebindProtectedParticipantOrSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database, ctx := databases[0], t.Context()
	source, target := compatibilityProfile(t, "a", "amd64"), compatibilityProfile(t, "b", "amd64")
	fixture := seedAgentSessionWithRuntimeBindings(t, pool, 212, workflow.RoleDeveloper, source.Binding(), source.Binding())
	result := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	session, err := database.GetAgentSession(ctx, fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.RuntimeProfileName != source.Binding().Name || session.RuntimeProfileVersion != source.Binding().Version || session.RuntimeProfileContentSHA256 != source.Binding().ContentSHA256 || session.RuntimeImageDigest != source.Binding().Image {
		t.Fatalf("qualification changed the protected Session: %+v", session)
	}
	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{source.Binding()})
}

func TestPiQualificationPersistsEveryExactAxis(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database, ctx := databases[0], t.Context()
	source, target := compatibilityProfile(t, "a", "amd64"), compatibilityProfile(t, "b", "amd64")
	baseline := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	mutations := []func(*runtimeprofile.CompatibilityResult){
		func(r *runtimeprofile.CompatibilityResult) { r.Source.ContentSHA256 = strings.Repeat("c", 64) },
		func(r *runtimeprofile.CompatibilityResult) { r.Source.Image = compatibilityImage("c") },
		func(r *runtimeprofile.CompatibilityResult) { r.Target.ContentSHA256 = strings.Repeat("c", 64) },
		func(r *runtimeprofile.CompatibilityResult) { r.Target.Image = compatibilityImage("c") },
		func(r *runtimeprofile.CompatibilityResult) { r.Platform.Arch = "arm64" },
		func(r *runtimeprofile.CompatibilityResult) { r.BackendFamily = "microsandbox" },
	}
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range mutations {
		result := baseline
		mutate(&result)
		if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
			t.Fatal(err)
		}
		key, err := result.QualificationKey()
		if err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT result FROM pi_runtime_profile_compatibility_results WHERE qualification_key = $1`, key).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var recorded runtimeprofile.CompatibilityResult
		if err := json.Unmarshal(raw, &recorded); err != nil || recorded != result {
			t.Fatalf("exact evidence axes changed: %+v %v", recorded, err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pi_runtime_profile_compatibility_results`).Scan(&count); err != nil || count != len(mutations)+1 {
		t.Fatalf("qualification axes collapsed: %d %v", count, err)
	}
}

func TestPiCompatibilityImportRejectsUnsupportedEvidenceAtomically(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database, ctx := databases[0], t.Context()
	source, target := compatibilityProfile(t, "a", "amd64"), compatibilityProfile(t, "b", "amd64")
	baseline := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	for _, mutate := range []func(*runtimeprofile.CompatibilityResult){
		func(r *runtimeprofile.CompatibilityResult) { r.Source.Name = "unsupported-runtime" },
		func(r *runtimeprofile.CompatibilityResult) { r.Source.Version = "v3" },
		func(r *runtimeprofile.CompatibilityResult) { r.Target.Name = "other-runtime" },
		func(r *runtimeprofile.CompatibilityResult) { r.Target.Version = "v3" },
		func(r *runtimeprofile.CompatibilityResult) { r.StateContractVersion = "unsupported-state/v1" },
		func(r *runtimeprofile.CompatibilityResult) {
			r.QualificationSuite = "unsupported-upgrade-suite"
		},
		func(r *runtimeprofile.CompatibilityResult) { r.BackendFamily = "" },
		func(r *runtimeprofile.CompatibilityResult) { r.BackendContractVersion = 2 },
	} {
		invalid := baseline
		mutate(&invalid)
		if err := database.RecordRuntimeProfileCompatibilityResult(ctx, invalid); err == nil {
			t.Fatal("unsupported evidence recorded")
		}
		file := runtimeprofile.CompatibilityResultsFile{SchemaVersion: runtimeprofile.PiCompatibilityResultsSchemaVersion, Results: []runtimeprofile.CompatibilityResult{baseline, invalid}}
		if err := database.ImportRuntimeProfileCompatibilityResults(ctx, file); err == nil {
			t.Fatal("invalid import accepted")
		}
	}
	file := runtimeprofile.CompatibilityResultsFile{SchemaVersion: "omnigrex.runtime-profile-compatibility-results/v1", Results: []runtimeprofile.CompatibilityResult{baseline}}
	if err := database.ImportRuntimeProfileCompatibilityResults(ctx, file); err == nil {
		t.Fatal("version 1 qualification artifact accepted")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pi_runtime_profile_compatibility_results`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid import retained partial evidence: %d %v", count, err)
	}
}

func TestRuntimeProfileCompatibilityResultPersistsAcrossStoreRestart(t *testing.T) {
	postgres := startPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.Open(ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	source, target := compatibilityProfile(t, "d", "amd64"), compatibilityProfile(t, "e", "amd64")
	result := newRuntimeProfileCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform, time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC))
	if err := first.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		first.Close()
		t.Fatal(err)
	}
	first.Close()
	second, err := store.Open(ctx, postgres.databaseURL(false), passwordFile, builtinStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if err := second.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatalf("idempotent result after Store restart: %v", err)
	}
	conflict := result
	conflict.QualifiedAt = "2026-09-06T13:00:01Z"
	if err := second.RecordRuntimeProfileCompatibilityResult(ctx, conflict); !errors.Is(err, store.ErrRuntimeProfileCompatibilityResultConflict) {
		t.Fatalf("restart lost immutable result: %v", err)
	}
}

func compatibilityPreparationSpec(commitSHA string, profile runtimeprofile.Profile) store.AgentTurnPreparationSpec {
	binding := profile.Binding()
	contract, _ := json.Marshal(profile.Contract())
	participant := func(name string, role workflow.Role, model string) store.ParticipantPreparation {
		content, _ := json.Marshal(map[string]any{"name": name, "role": role, "path": ".omnigrex/team/" + name + ".md", "runtime": binding.Name + "/" + binding.Version, "model": model, "permissions": map[string]string{"read": "allow"}, "instructions": "Perform the assigned Work Item."})
		hash := sha256.Sum256(content)
		return store.ParticipantPreparation{
			ProfilePath:     ".omnigrex/team/" + name + ".md",
			Binding:         store.ParticipantRuntimeBinding{AgentProfileName: name, RuntimeProfileName: binding.Name, RuntimeProfileVersion: binding.Version, RuntimeProfileContentSHA256: binding.ContentSHA256, RuntimeImageDigest: binding.Image},
			RuntimeContract: contract,
			Profile:         store.AgentProfileSnapshot{CommitSHA: commitSHA, ContentSHA256: hash[:], Config: content},
		}
	}
	return store.AgentTurnPreparationSpec{Developer: participant("developer", workflow.RoleDeveloper, "openai/compatibility"), Reviewer: participant("reviewer", workflow.RoleReviewer, "anthropic/compatibility")}
}

func compatibilityProfile(t *testing.T, digit, architecture string) runtimeprofile.Profile {
	t.Helper()
	profile, err := runtimeprofile.NewPiV2(compatibilityImage(digit), runtimeprofile.Platform{OS: "linux", Arch: architecture})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func newRuntimeProfileCompatibilityResult(source, target runtimeprofile.Binding, platform runtimeprofile.Platform, qualifiedAt time.Time) runtimeprofile.CompatibilityResult {
	return runtimeprofile.CompatibilityResult{Source: source, Target: target, Platform: platform, StateContractVersion: runtimeprofile.PiStateContractVersion, WorkspacePath: runtimeprofile.StableWorkspacePath, QualificationSuite: runtimeprofile.PiCompatibilityQualificationSuite, QualificationVersion: runtimeprofile.CompatibilityQualificationVersion, QualifiedAt: qualifiedAt.UTC().Truncate(time.Second).Format(time.RFC3339), Outcome: "success", BackendFamily: "docker", BackendContractVersion: 1}
}

func compatibilityImage(digit string) string {
	return "registry.example/omnigrex/pi@sha256:" + strings.Repeat(digit, 64)
}

func compatibilityUUID(prefix, sequence int) string {
	return fmt.Sprintf("%02d000000-0000-4000-8000-%012d", prefix, sequence)
}
