package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jozala/omnigrex/internal/server"
)

func TestHealthEndpointsReportReady(t *testing.T) {
	readinessChecked := false
	handler := server.Handler(server.ReadinessFunc(func(context.Context) error {
		readinessChecked = true
		return nil
	}), nil)
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
	if !readinessChecked {
		t.Error("readiness checker was not called")
	}
}

func TestLivenessDoesNotCheckReadiness(t *testing.T) {
	readinessChecked := false
	handler := server.Handler(server.ReadinessFunc(func(context.Context) error {
		readinessChecked = true
		return errors.New("dependency unavailable")
	}), nil)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.String() != "ok\n" {
		t.Errorf("body = %q, want %q", response.Body.String(), "ok\n")
	}
	if readinessChecked {
		t.Error("readiness checker was called for liveness request")
	}
}

func TestReadinessFailureReturnsUnavailableWithoutLeakingError(t *testing.T) {
	handler := server.Handler(server.ReadinessFunc(func(context.Context) error {
		return errors.New("database password was secret")
	}), nil)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if response.Body.String() != "not ready\n" {
		t.Errorf("body = %q, want %q", response.Body.String(), "not ready\n")
	}
}

func TestReadinessReceivesRequestContext(t *testing.T) {
	type contextKey struct{}
	const contextValue = "request value"

	var got any
	handler := server.Handler(server.ReadinessFunc(func(ctx context.Context) error {
		got = ctx.Value(contextKey{})
		return nil
	}), nil)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	request = request.WithContext(context.WithValue(request.Context(), contextKey{}, contextValue))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got != contextValue {
		t.Errorf("checker context value = %v, want %q", got, contextValue)
	}
}

func TestHealthEndpointsRejectPost(t *testing.T) {
	readinessChecked := false
	handler := server.Handler(server.ReadinessFunc(func(context.Context) error {
		readinessChecked = true
		return nil
	}), nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
			}
		})
	}
	if readinessChecked {
		t.Error("readiness checker was called for POST request")
	}
}

func TestGitHubWebhookRouteDelegatesOnlyPost(t *testing.T) {
	calls := 0
	webhook := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodPost || request.URL.Path != "/webhooks/github" {
			t.Errorf("webhook request = %s %s", request.Method, request.URL.Path)
		}
		response.WriteHeader(http.StatusAccepted)
	})
	handler := server.Handler(server.ReadinessFunc(func(context.Context) error { return nil }), webhook)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/webhooks/github", nil))
	if response.Code != http.StatusAccepted || calls != 1 {
		t.Errorf("POST webhook = status %d, calls %d; want 202, 1", response.Code, calls)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/webhooks/github", nil))
	if response.Code != http.StatusMethodNotAllowed || calls != 1 {
		t.Errorf("GET webhook = status %d, calls %d; want 405, 1", response.Code, calls)
	}
}
