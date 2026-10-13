//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestInstallationRuntimeCompatibilityRejectsCollectedUnsupportedHistory(t *testing.T) {
	for _, test := range []struct {
		name        string
		participant bool
		session     bool
		contract    bool
	}{
		{name: "fresh and Pi history"},
		{name: "collected OpenCode Participant", participant: true},
		{name: "collected OpenCode Session", session: true},
		{name: "orphaned OpenCode contract", contract: true},
		{name: "collected OpenCode ledger", participant: true, session: true, contract: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			candidate, err := profile.NewPiV1("registry.example/omnigrex/pi@sha256:"+strings.Repeat("b", 64), profile.Platform{OS: "linux", Arch: "amd64"})
			if err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(t.TempDir(), "database-password")
			if err := os.WriteFile(secret, []byte(postgresPassword), 0o600); err != nil {
				t.Fatal(err)
			}
			readOnly, err := store.OpenReadOnly(ctx, pool.Config().ConnString(), secret)
			if err != nil {
				t.Fatal(err)
			}
			defer readOnly.Close()
			checks := []struct {
				name  string
				check func() error
			}{
				{name: "writable", check: func() error { return database.CheckPendingRuntimeProfileCompatibility(ctx, candidate) }},
				{name: "read-only", check: func() error { return readOnly.CheckPendingRuntimeProfileCompatibility(ctx, candidate) }},
			}
			for _, check := range checks {
				if err := check.check(); err != nil {
					t.Fatalf("%s fresh installation: %v", check.name, err)
				}
			}
			// A different Pi image remains valid for a fresh Participant or upgrade.
			source, err := profile.NewPiV1("registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64), candidate.Contract().Platform)
			if err != nil {
				t.Fatal(err)
			}
			participant, session := source.Binding(), source.Binding()
			if test.participant {
				participant.Name = "opencode-acp"
			}
			if test.session {
				session.Name = "opencode-acp"
			}
			fixture := seedAgentSessionWithRuntimeBindingsAndStatus(t, pool, 2401, workflow.RoleDeveloper, participant, session, store.AgentSessionCreating)
			if _, err := pool.Exec(ctx, `UPDATE agent_assignments
SET status = 'COMPLETED', completed_at = clock_timestamp(), state_deleted_at = clock_timestamp()
WHERE id = $1`, fixture.assignmentID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_sessions
SET status = 'DELETED', state_deleted_at = clock_timestamp() WHERE id = $1`, fixture.sessionID); err != nil {
				t.Fatal(err)
			}
			contract := source.Contract()
			if test.contract {
				contract.Name = "opencode-acp"
			}
			content, err := json.Marshal(contract)
			if err != nil {
				t.Fatal(err)
			}
			contractHash := sha256.Sum256(content)
			if _, err := pool.Exec(ctx, `INSERT INTO runtime_profile_contracts (content_sha256, contract) VALUES ($1, $2)`, hex.EncodeToString(contractHash[:]), content); err != nil {
				t.Fatal(err)
			}
			if bindings, err := database.ListProtectedRuntimeBindings(ctx); err != nil || len(bindings) != 0 {
				t.Fatalf("collected installation unexpectedly has protected bindings: %v, %v", bindings, err)
			}
			unsupported := test.participant || test.session || test.contract
			for _, check := range checks {
				err := check.check()
				if unsupported && !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) || !unsupported && err != nil {
					t.Fatalf("%s collected installation check = %v, unsupported %t", check.name, err, unsupported)
				}
			}
			var preserved bool
			if err := pool.QueryRow(ctx, `SELECT participant.runtime_profile_name = $3
    AND session.runtime_profile_name = $4
    AND participant.state_deleted_at IS NOT NULL AND session.state_deleted_at IS NOT NULL
FROM agent_assignments AS participant JOIN agent_sessions AS session ON session.agent_assignment_id = participant.id
WHERE participant.id = $1 AND session.id = $2`, fixture.assignmentID, fixture.sessionID, participant.Name, session.Name).Scan(&preserved); err != nil || !preserved {
				t.Fatalf("installation check changed historical rows: preserved %t, error %v", preserved, err)
			}
		})
	}
}
