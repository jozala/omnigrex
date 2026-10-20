package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/providerauth"
	"github.com/jozala/omnigrex/internal/store"
)

type runtimeCredentials struct {
	access  func() (providerauth.AccessToken, error)
	reports int
}

func (p *runtimeCredentials) Access(context.Context) (providerauth.AccessToken, error) {
	return p.access()
}
func (p *runtimeCredentials) Report(string, string) error { p.reports++; return nil }

func TestRuntimeCredentialsRequireLiveOpenAITurnAuthority(t *testing.T) {
	for _, scenario := range []string{"valid", "missing-token", "revoked", "expired", "fenced", "fence-lost-during-refresh", "different-provider", "quota"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now()
			durable := &fakeStore{}
			provider := &runtimeCredentials{access: func() (providerauth.AccessToken, error) {
				return providerauth.AccessToken{Access: "private-access-token", ExpiresAt: now.Add(time.Hour), Generation: "generation"}, nil
			}}
			gateway, err := mcp.New(mcp.Config{EndpointURL: "http://gateway.test/mcp", Store: durable, Backend: fakeBackend{}, ProviderAuth: provider, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			scope := validScope(now)
			scope.Lease.AgentProfileConfig = json.RawMessage(`{"role":"DEVELOPER","name":"developer","model":"openai/gpt-6.1-sol"}`)
			if scenario == "different-provider" {
				scope.Lease.AgentProfileConfig = json.RawMessage(`{"role":"DEVELOPER","name":"developer","model":"other/model"}`)
			}
			registration, err := gateway.Register(scope)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/runtime/provider-auth", nil)
			request.Header.Set("Authorization", registration.Server.Headers[0].Value)
			want := http.StatusUnauthorized
			switch scenario {
			case "valid":
				want = http.StatusOK
			case "missing-token":
				request.Header.Del("Authorization")
			case "revoked":
				gateway.Revoke(registration)
			case "expired":
				now = now.Add(2 * time.Hour)
			case "fenced":
				durable.setValidateError(store.ErrAgentTurnFenceLost)
			case "fence-lost-during-refresh":
				provider.access = func() (providerauth.AccessToken, error) {
					durable.setValidateError(store.ErrAgentTurnFenceLost)
					return providerauth.AccessToken{Access: "private-access-token"}, nil
				}
			case "different-provider":
				want = http.StatusForbidden
			case "quota":
				want = http.StatusServiceUnavailable
				provider.access = func() (providerauth.AccessToken, error) { return providerauth.AccessToken{}, providerauth.ErrQuota }
			}
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, request)
			if response.Code != want {
				t.Fatalf("status = %d, want %d", response.Code, want)
			}
			if scenario == "valid" {
				if !strings.Contains(response.Body.String(), "private-access-token") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("private token response lost token or cache protection")
				}
			} else if strings.Contains(response.Body.String(), "private-access-token") {
				t.Fatal("credential leaked to an unauthorized request")
			}
			if scenario == "quota" && !strings.Contains(response.Body.String(), `"failure":"subscription_quota"`) {
				t.Fatal("lost safe quota classification")
			}
		})
	}
}

func TestRuntimeCredentialFailureReportsUseClosedVocabulary(t *testing.T) {
	now := time.Now()
	provider := &runtimeCredentials{}
	gateway, err := mcp.New(mcp.Config{EndpointURL: "http://gateway.test/mcp", Store: &fakeStore{}, Backend: fakeBackend{}, ProviderAuth: provider, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(now)
	scope.Lease.AgentProfileConfig = json.RawMessage(`{"role":"DEVELOPER","name":"developer","model":"openai/fixture"}`)
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"generation":"fixture","failure":"subscription_quota"}`, `{"generation":"fixture","failure":"provider-secret"}`, `{"generation":"fixture","failure":"subscription_auth","secret":"untrusted"}`} {
		request := httptest.NewRequest(http.MethodPost, "/runtime/provider-auth", strings.NewReader(body))
		request.Header.Set("Authorization", registration.Server.Headers[0].Value)
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, request)
		if strings.Contains(response.Body.String(), "secret") {
			t.Fatal("echoed untrusted failure details")
		}
	}
	if provider.reports != 1 {
		t.Fatalf("accepted reports = %d", provider.reports)
	}
}

func TestRuntimeCredentialsAreUnavailableUnderMCPPath(t *testing.T) {
	now := time.Now()
	accessCalls := 0
	provider := &runtimeCredentials{access: func() (providerauth.AccessToken, error) {
		accessCalls++
		return providerauth.AccessToken{Access: "private-access-token"}, nil
	}}
	gateway, err := mcp.New(mcp.Config{EndpointURL: "http://gateway.test/mcp", Store: &fakeStore{}, Backend: fakeBackend{}, ProviderAuth: provider, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(now)
	scope.Lease.AgentProfileConfig = json.RawMessage(`{"role":"DEVELOPER","name":"developer","model":"openai/fixture"}`)
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request := httptest.NewRequest(method, "/mcp/provider-auth", strings.NewReader(`{"generation":"fixture","failure":"subscription_quota"}`))
		request.Header.Set("Authorization", registration.Server.Headers[0].Value)
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "private-access-token") {
			t.Fatalf("old runtime path response = %d %s", response.Code, response.Body.String())
		}
	}
	if accessCalls != 0 || provider.reports != 0 {
		t.Fatal("MCP path invoked runtime credential operations")
	}
}
