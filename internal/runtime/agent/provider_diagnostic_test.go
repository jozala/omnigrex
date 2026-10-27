package agent_test

import (
	"encoding/json"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"strings"
	"testing"
)

func TestProviderDiagnosticDropsUntrustedFieldsWithoutErasingKnownEvidence(t *testing.T) {
	for _, raw := range []string{
		`{"stage":"provider_http","http_status":502,"code":"SENTINEL","cause":"http","request_id_hash":"SENTINEL","attempt":"SENTINEL","message":"SENTINEL"}`,
		`{"stage":"credential","http_status":503,"code":"subscription_sharing_invalid_user"}`,
		`{`, `null`, `[]`, `{"stage":"SENTINEL","elapsed_ms":-2,"http_status":900}`, strings.Repeat("SENTINEL", 1000),
	} {
		d := agent.ParseProviderDiagnostic(json.RawMessage(raw))
		encoded, err := json.Marshal(d)
		if err != nil || strings.Contains(string(encoded), "SENTINEL") || strings.Contains(d.Summary(), "SENTINEL") {
			t.Fatalf("unsafe diagnostic: %s, %v", encoded, err)
		}
		if strings.Contains(raw, `"provider_http"`) && (d.Stage != "provider_http" || d.HTTPStatus == nil || *d.HTTPStatus != 502 || d.Code != "unknown") {
			t.Fatalf("lost evidence: %s", encoded)
		}
		if strings.Contains(raw, `"credential"`) && (d.Stage != "credential" || d.Code != "subscription_sharing_invalid_user" || *d.HTTPStatus != 503) {
			t.Fatalf("lost credential evidence: %s", encoded)
		}
	}
}

func TestProviderDiagnosticCategoryIsClosedAndRevalidated(t *testing.T) {
	for _, category := range []string{"subscription_temporary", "subscription_quota", "subscription_auth", "subscription_configuration", "SENTINEL"} {
		d := agent.ParseProviderDiagnostic(json.RawMessage(`{"category":"` + category + `","stage":"provider_http","http_status":502}`))
		if d.Stage != "provider_http" || d.HTTPStatus == nil || *d.HTTPStatus != 502 {
			t.Fatalf("category erased HTTP evidence: %+v", d)
		}
		if category == "SENTINEL" {
			if d.Category != "" || strings.Contains(d.Summary(), "SENTINEL") {
				t.Fatalf("unsafe category: %+v", d)
			}
		} else if d.Category != category || !strings.Contains(d.Summary(), "category="+category) {
			t.Fatalf("lost category: %+v", d)
		}
	}
}
