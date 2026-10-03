package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
	} {
		t.Setenv(name, "")
	}
	provider, propagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(provider)
		otel.SetTextMapPropagator(propagator)
	})
}

func TestInitInactive(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "no endpoint", true: "SDK disabled"}[disabled], func(t *testing.T) {
			cleanEnvironment(t)
			if disabled {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
				t.Setenv("OTEL_SDK_DISABLED", "true")
			}
			previous := otel.GetTracerProvider()
			shutdown, err := Init(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			if otel.GetTracerProvider() != previous {
				t.Fatal("inactive tracing replaced the global provider")
			}
			if err := shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInitExportsAndFlushes(t *testing.T) {
	for _, traceEndpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic endpoint", true: "trace endpoint"}[traceEndpoint], func(t *testing.T) {
			cleanEnvironment(t)
			received := make(chan *collector.ExportTraceServiceRequest, 1)
			path := "/otlp/v1/traces"
			if traceEndpoint {
				path = "/custom-traces"
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path || r.Header.Get("Authorization") != "Basic test" {
					t.Errorf("unexpected export path or authentication: %s", r.URL.Path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				request := new(collector.ExportTraceServiceRequest)
				if err := proto.Unmarshal(body, request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- request
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer backend.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", backend.URL+"/otlp")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Basic%20test")
			if traceEndpoint {
				t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", backend.URL+path)
			}
			t.Setenv("OTEL_SERVICE_NAME", "custom-omnigrex")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test")
			t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
			shutdown, err := Init(context.Background(), "test-version")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = shutdown(context.Background()) }()
			carrier := propagation.MapCarrier{"traceparent": "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01", "baggage": "example=value"}
			ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
			ctx, span := otel.Tracer("test").Start(ctx, "operation")
			outgoing := propagation.MapCarrier{}
			otel.GetTextMapPropagator().Inject(ctx, outgoing)
			if outgoing.Get("baggage") != "example=value" {
				t.Fatal("baggage was not propagated")
			}
			span.End()
			// A fresh context lets shutdown flush even after the service context is canceled.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-received:
				if len(request.ResourceSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans) != 1 {
					t.Fatalf("unexpected resource spans: %v", request)
				}
				resourceSpans := request.ResourceSpans[0]
				attributes := map[string]string{}
				for _, attr := range resourceSpans.Resource.Attributes {
					attributes[attr.Key] = attr.Value.GetStringValue()
				}
				if attributes["service.name"] != "custom-omnigrex" || attributes["service.version"] != "test-version" || attributes["deployment.environment.name"] != "test" {
					t.Fatalf("unexpected resource attributes: %v", attributes)
				}
				spans := resourceSpans.ScopeSpans[0].Spans
				parent := trace.SpanContextFromContext(otel.GetTextMapPropagator().Extract(context.Background(), carrier))
				traceID, spanID := parent.TraceID(), parent.SpanID()
				if len(spans) != 1 || spans[0].Name != "operation" || !bytes.Equal(spans[0].TraceId, traceID[:]) || !bytes.Equal(spans[0].ParentSpanId, spanID[:]) {
					t.Fatalf("unexpected exported spans: %v", spans)
				}
			case <-shutdownCtx.Done():
				t.Fatal("shutdown did not export the completed span")
			}
		})
	}
}
