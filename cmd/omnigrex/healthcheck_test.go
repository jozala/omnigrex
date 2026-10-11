package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/server"
)

func TestHealthcheckAllowsConfiguredReadinessBudget(t *testing.T) {
	const budget = 60 * time.Second
	readiness := combinedReadiness(budget, func(ctx context.Context) error {
		timer := time.NewTimer(21 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	endpoint := httptest.NewServer(server.Handler(readiness, nil))
	defer endpoint.Close()
	t.Setenv("OMNIGREX_HEALTHCHECK_URL", endpoint.URL+"/readyz")
	t.Setenv("OMNIGREX_READINESS_TIMEOUT", "60s")
	if code := healthcheck(); code != 0 {
		t.Fatalf("readiness completed inside its configured budget but healthcheck exited %d", code)
	}
}

func TestHealthcheckStalledReadinessIsBounded(t *testing.T) {
	readiness := combinedReadiness(50*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	endpoint := httptest.NewServer(server.Handler(readiness, nil))
	defer endpoint.Close()
	t.Setenv("OMNIGREX_HEALTHCHECK_URL", endpoint.URL+"/readyz")
	t.Setenv("OMNIGREX_READINESS_TIMEOUT", "50ms")
	start := time.Now()
	if code := healthcheck(); code != 1 {
		t.Fatalf("stalled readiness returned healthcheck exit %d", code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("readiness handler exceeded its bounded budget: %s", elapsed)
	}
}

func TestHealthcheckStalledHTTPPeerIsBounded(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer endpoint.Close()
	t.Setenv("OMNIGREX_HEALTHCHECK_URL", endpoint.URL+"/readyz")
	t.Setenv("OMNIGREX_READINESS_TIMEOUT", "50ms")
	start := time.Now()
	if code := healthcheck(); code != 1 {
		t.Fatalf("stalled HTTP peer returned healthcheck exit %d", code)
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("HTTP client did not enforce readiness plus response allowance: %s", elapsed)
	}
}

func TestHealthcheckRejectsInvalidReadinessBudget(t *testing.T) {
	for _, value := range []string{"invalid", "0s", "-1s", "2562047h47m16s"} {
		t.Run(value, func(t *testing.T) {
			var called atomic.Bool
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				writer.WriteHeader(http.StatusOK)
			}))
			defer endpoint.Close()
			t.Setenv("OMNIGREX_HEALTHCHECK_URL", endpoint.URL)
			t.Setenv("OMNIGREX_READINESS_TIMEOUT", value)
			if code := healthcheck(); code != 1 || called.Load() {
				t.Fatalf("invalid budget: exit=%d HTTP request=%t", code, called.Load())
			}
		})
	}
}
