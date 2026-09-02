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
	}))
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
	}))
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
	}))
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
	}))
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
	}))

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
