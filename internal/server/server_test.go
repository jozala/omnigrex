package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jozala/omnigrex/internal/server"
)

func TestHealthEndpointsReportReady(t *testing.T) {
	handler := server.Handler()
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if response.Body.String() != "ok\n" {
				t.Errorf("body = %q, want %q", response.Body.String(), "ok\n")
			}
		})
	}
}

func TestHealthEndpointsRejectPost(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}
