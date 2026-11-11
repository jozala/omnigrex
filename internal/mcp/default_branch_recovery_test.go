package mcp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
)

func TestSucceededDefaultSnapshotRecoveryValidatesHistoricalReceipt(t *testing.T) {
	scope := reconciliationContext()
	scope.Turn.AgentSessionID = "session-1"
	mutation := reconciliationMutation(mcp.ToolRefreshDefaultBranch, `{"operation_id":"caller-key"}`)
	mutation.State = store.MutationSucceeded
	mutation.ExternalService = "workspace"
	mutation.ExternalResourceID = "9123:assignment-1:session-1:turn-1:4"
	digest := sha256.Sum256([]byte(mutation.ExternalResourceID + ":" + mutation.ID))
	receipt := map[string]any{"status": "current", "repository": map[string]any{"id": 9123, "owner": "acme", "name": "widgets"}, "branch": "main", "sha": productionHeadSHA, "ref": "refs/omnigrex/default/" + hex.EncodeToString(digest[:]), "observed_at": time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), "reobserved_at": time.Date(2026, 11, 11, 0, 0, 1, 0, time.UTC), "observed_branch": "main", "observed_sha": productionHeadSHA}
	// Even a plausible receipt cannot promote an interrupted installation.
	for _, state := range []store.MutationState{store.MutationUnknown, store.MutationReconciling} {
		m := mutation
		m.State = state
		m.Result, _ = json.Marshal(receipt)
		r := newProductionReconciler(t, &reconciliationGitHub{}, &reconciliationPublications{})
		got, err := r.Reconcile(t.Context(), scope, m)
		if err != nil || got.Disposition != mcp.ReconciliationUnresolved || len(got.Outcome.Result) != 0 {
			t.Fatalf("interrupted state %s inferred success: %+v %v", state, got, err)
		}
	}
	for _, scenario := range []string{"current", "stale", "changed-installed", "changed-before-install", "unavailable", "unavailable-after-install", "wrong-resource", "wrong-ref", "wrong-repository", "wrong-epoch", "wrong-operation", "missing-result", "malformed-result", "missing-observation", "missing-reobservation", "inconsistent-current"} {
		t.Run(scenario, func(t *testing.T) {
			m := mutation
			r := map[string]any{}
			for k, v := range receipt {
				r[k] = v
			}
			valid := true
			switch scenario {
			case "current":
			case "stale":
				r["status"] = "stale"
				r["observed_sha"] = advancedHeadSHA
			case "changed-installed":
				r["status"] = "default_branch_changed"
				r["observed_branch"] = "trunk"
			case "changed-before-install":
				r["status"] = "default_branch_changed"
				r["observed_branch"] = "trunk"
				delete(r, "sha")
				delete(r, "ref")
				delete(r, "reobserved_at")
			case "unavailable":
				r["status"] = "unavailable"
				r["reason"] = "credential_unavailable"
				for _, k := range []string{"sha", "ref", "observed_sha", "observed_branch", "reobserved_at"} {
					delete(r, k)
				}
			case "unavailable-after-install":
				r["status"] = "unavailable"
				r["reason"] = "reobservation_unavailable"
			case "wrong-resource":
				m.ExternalResourceID = "9123:other:session-1:turn-1:4"
				valid = false
			case "wrong-ref":
				r["ref"] = "refs/omnigrex/default/partial"
				valid = false
			case "wrong-repository":
				r["repository"] = map[string]any{"id": 999, "owner": "acme", "name": "widgets"}
				valid = false
			case "wrong-epoch":
				m.ExecutionEpoch++
				valid = false
			case "wrong-operation":
				m.OperationID = "different"
				valid = false
			case "missing-result", "malformed-result":
				valid = false
			case "missing-observation":
				delete(r, "observed_at")
				valid = false
			case "missing-reobservation":
				delete(r, "reobserved_at")
				valid = false
			case "inconsistent-current":
				r["observed_sha"] = advancedHeadSHA
				valid = false
			}
			m.Result, _ = json.Marshal(r)
			if scenario == "missing-result" {
				m.Result = nil
			}
			if scenario == "malformed-result" {
				m.Result = []byte(`{"fixture_success":true}`)
			}
			api := &reconciliationGitHub{}
			reconciler := newProductionReconciler(t, api, &reconciliationPublications{})
			result, err := reconciler.Reconcile(t.Context(), scope, m)
			if valid {
				if err != nil || result.Disposition != mcp.ReconciliationFound || string(result.Outcome.Result) != string(m.Result) {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil {
				t.Fatalf("invalid receipt accepted: %+v", result)
			}
			if api.credential != "" {
				t.Fatal("historical receipt caused external observation")
			}
		})
	}
}
