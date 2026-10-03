package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	mobyclient "github.com/moby/moby/client"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestReadinessDockerClientsDoNotTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})

	var requests []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("API-Version", "1.54")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		case strings.Contains(r.URL.Path, "/images/"):
			_, _ = w.Write([]byte(`{"Id":"sha256:test","Os":"linux","Architecture":"amd64"}`))
		case strings.Contains(r.URL.Path, "/networks/"):
			_, _ = w.Write([]byte(`{"Id":"network","Name":"omnigrex-agent"}`))
		case strings.Contains(r.URL.Path, "/volumes/"):
			_, _ = w.Write([]byte(`{"Name":"volume"}`))
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer daemon.Close()
	t.Setenv("DOCKER_HOST", daemon.URL)
	t.Setenv("DOCKER_API_VERSION", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")

	probe, err := NewReadinessProbe(testReadinessProbeOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	if err := probe.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 6 {
		t.Fatalf("readiness issued %d requests, want ping, image, network, and three volumes", len(requests))
	}

	availability, err := NewExactImageAvailability()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = availability.Close() }()
	image := "registry.example/opencode@sha256:" + strings.Repeat("a", 64)
	if err := availability.Available(context.Background(), image, runtimeprofile.Platform{OS: "linux", Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) <= 6 {
		t.Fatal("image availability did not call Docker")
	}
	if spans := recorder.Ended(); len(spans) != 0 {
		t.Fatalf("diagnostic Docker clients emitted %d spans", len(spans))
	}

	// Other Docker clients must still use the global trace provider.
	operational, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = operational.Close() }()
	if _, err := operational.Ping(context.Background(), mobyclient.PingOptions{}); err != nil {
		t.Fatal(err)
	}
	if spans := recorder.Ended(); len(spans) == 0 {
		t.Fatal("operational Docker client tracing was disabled")
	}
}
