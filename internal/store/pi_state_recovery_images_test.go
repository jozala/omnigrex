//go:build integration

package store_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestPiRecoveryLocalImageAvailability(t *testing.T) {
	local := "sha256:" + strings.Repeat("a", 64)
	expected := "registry.example/omnigrex/pi@" + local
	for _, tc := range []struct {
		name, os, arch string
		wantErr        error
	}{
		{name: "matching platform", os: "linux", arch: "amd64"},
		{name: "wrong OS", os: "windows", arch: "amd64", wantErr: execution.ErrBindingMismatch},
		{name: "wrong architecture", os: "linux", arch: "arm64", wantErr: execution.ErrBindingMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1.48/images/"+local+"/json" || r.URL.RawQuery != "" {
					t.Errorf("unexpected image inspection: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]string{"Id": local, "Os": tc.os, "Architecture": tc.arch}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", server.URL)
			t.Setenv("DOCKER_API_VERSION", "1.48")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			images := piRecoveryImages{expected: expected, local: local}
			err := images.Available(t.Context(), expected, profile.Platform{OS: "linux", Arch: "amd64"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Available() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
