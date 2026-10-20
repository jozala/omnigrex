package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/jozala/omnigrex/internal/providerauth"
)

// RuntimeCredentialProvider is deliberately separate from the model-visible
// tool protocol. Existing per-Turn authority scopes access and failure reports.
type RuntimeCredentialProvider interface {
	Access(context.Context) (providerauth.AccessToken, error)
	Report(string, string) error
}

func (gateway *Gateway) serveProviderAuth(w http.ResponseWriter, r *http.Request, registration *grant) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	var profile struct {
		Model string `json:"model"`
	}
	if gateway.providerAuth == nil || json.Unmarshal(registration.scope.Lease.AgentProfileConfig, &profile) != nil || !strings.HasPrefix(profile.Model, "openai/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		token, err := gateway.providerAuth.Access(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(struct {
				Failure string `json:"failure"`
			}{Failure: providerauth.Failure(err)})
			return
		}
		// Renewal may have waited for a remote response. Recheck authorization
		// and durable fencing before returning the resulting credential.
		if !gateway.grantLive(registration) || gateway.store.ValidateTurnFence(r.Context(), registration.scope.Lease) != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(token)
	case http.MethodPost:
		var report struct {
			Generation string `json:"generation"`
			Failure    string `json:"failure"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		d.DisallowUnknownFields()
		if d.Decode(&report) != nil || d.Decode(&struct{}{}) != io.EOF || report.Generation == "" || (report.Failure != providerauth.QuotaFailure && report.Failure != providerauth.AuthFailure) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := gateway.providerAuth.Report(report.Generation, report.Failure); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "{}\n")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
