package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestMetricsOnlyPeriodicExportAndScopeAllowlist(t *testing.T) {
	cleanEnvironment(t)
	received := make(chan *collector.ExportMetricsServiceRequest, 30)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom-metrics" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("incorrect metrics endpoint/headers")
		}
		body, _ := io.ReadAll(r.Body)
		request := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
		}
		select {
		case received <- request:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer backend.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", backend.URL+"/custom-metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=wrong")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "Authorization=Bearer%20test")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20")
	previousTrace := otel.GetTracerProvider()
	shutdown, err := Init(context.Background(), "build-test")
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != previousTrace {
		t.Fatal("metrics-only changed trace provider")
	}
	unregister, err := RegisterStateObserver(func(context.Context) (DurableState, error) {
		return DurableState{ActiveTurns: 3}, nil
	})
	if err != nil {
		_ = shutdown(context.Background())
		t.Fatal(err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Error(err)
		}
		unregister()
	}()
	unplanned, err := otel.Meter("third-party/http").Int64Counter("unplanned.requests")
	if err != nil {
		t.Fatal(err)
	}
	unplanned.Add(context.Background(), 1)
	_, operation := StartOperation(context.Background(), AgentTurnExecute)
	operation.Finish(new(error))
	deadline := time.After(3 * time.Second)
	for {
		select {
		case request := <-received:
			found := false
			for _, resource := range request.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					if scope.Scope.Name != Scope {
						t.Fatalf("unplanned scope exported: %s", scope.Scope.Name)
					}
					for _, metric := range scope.Metrics {
						if metric.Name == "omnigrex.agent_turn.active" {
							found = true
							if metric.GetGauge().DataPoints[0].GetAsInt() != 3 {
								t.Fatal("incorrect durable gauge")
							}
						}
					}
				}
			}
			if found {
				return
			}
		case <-deadline:
			t.Fatal("metrics did not export periodically")
		}
	}
}

func TestMetricsEnablementMatrix(t *testing.T) {
	for _, test := range []struct {
		name, generic, traces, metrics, selector, disabled string
		wantTraces, wantMetrics, wantError                 bool
	}{
		{name: "unconfigured"},
		{name: "existing generic remains tracing only", generic: "http://127.0.0.1:4318", wantTraces: true},
		{name: "trace specific", traces: "http://127.0.0.1:4318/v1/traces", wantTraces: true},
		{name: "metrics specific", metrics: "http://127.0.0.1:4318/v1/metrics", wantMetrics: true},
		{name: "both", generic: "http://127.0.0.1:4318", selector: "otlp", wantTraces: true, wantMetrics: true},
		{name: "metrics disabled", generic: "http://127.0.0.1:4318", metrics: "http://127.0.0.1:4318/v1/metrics", selector: "none", wantTraces: true},
		{name: "SDK disabled", generic: "http://127.0.0.1:4318", selector: "otlp", disabled: "true"},
		{name: "missing metrics endpoint", selector: "otlp", wantError: true},
		{name: "unsupported exporter", selector: "prometheus", wantError: true},
		{name: "invalid metrics endpoint", metrics: "secret-invalid", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", test.generic)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", test.traces)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", test.metrics)
			t.Setenv("OTEL_METRICS_EXPORTER", test.selector)
			t.Setenv("OTEL_SDK_DISABLED", test.disabled)
			traceBefore, meterBefore := otel.GetTracerProvider(), otel.GetMeterProvider()
			shutdown, err := Init(context.Background(), "matrix")
			if (err != nil) != test.wantError {
				t.Fatalf("initialization error: %v", err)
			}
			if err != nil {
				return
			}
			if (otel.GetTracerProvider() != traceBefore) != test.wantTraces || (otel.GetMeterProvider() != meterBefore) != test.wantMetrics {
				t.Fatal("incorrect enabled signals")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_ = shutdown(ctx)
		})
	}
}

func TestDurableObservationFailureOmitsGaugesAndRecovers(t *testing.T) {
	cleanEnvironment(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(applicationMetricView))
	otel.SetMeterProvider(provider)
	metricsActive.Store(true)
	t.Cleanup(func() { metricsActive.Store(false); _ = provider.Shutdown(context.Background()) })
	fail := false
	unregister, err := RegisterStateObserver(func(context.Context) (DurableState, error) {
		if fail {
			return DurableState{}, errors.New("secret-database-error")
		}
		return DurableState{ActiveTurns: 2, OldestUnresolvedSeconds: -1}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	for _, failed := range []bool{false, true, false} {
		fail = failed
		var data metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &data); err != nil {
			t.Fatal(err)
		}
		gauges := 0
		for _, scope := range data.ScopeMetrics {
			for _, instrument := range scope.Metrics {
				switch value := instrument.Data.(type) {
				case metricdata.Gauge[int64]:
					gauges += len(value.DataPoints)
				case metricdata.Gauge[float64]:
					gauges += len(value.DataPoints)
					for _, point := range value.DataPoints {
						if point.Value < 0 {
							t.Fatal("negative age exported")
						}
					}
				case metricdata.Sum[int64]:
					if failed && (instrument.Name != "omnigrex.observation.failures" || value.DataPoints[0].Value != 1) {
						t.Fatal("missing failure count")
					}
				}
			}
		}
		if failed && gauges != 0 || !failed && gauges != 26 {
			t.Fatalf("gauges=%d after failure=%v", gauges, failed)
		}
	}
}

func TestApplicationMetricSeriesBudgetAndAttributeAllowlist(t *testing.T) {
	cleanEnvironment(t)
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(applicationMetricView))
	otel.SetMeterProvider(provider)
	metricsActive.Store(true)
	t.Cleanup(func() { metricsActive.Store(false); _ = provider.Shutdown(context.Background()) })
	unregister, err := RegisterStateObserver(func(context.Context) (DurableState, error) { return DurableState{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	for _, name := range operationNames {
		for _, outcome := range []Outcome{Success, DomainOutcome, Failure, Cancelled, Timeout} {
			recordOperation(context.Background(), name, outcome, time.Second)
		}
	}
	recordOperation(context.Background(), OperationName("unbounded-name"), Success, time.Second)
	recordOperation(context.Background(), AgentTurnExecute, Outcome("unbounded-outcome"), time.Second)
	// Even attributes accidentally supplied by an application caller are dropped.
	counter, _ := otel.Meter(Scope).Int64Counter("omnigrex.operation.attempts", metric.WithUnit("{attempt}"))
	counter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("operation", "agent_turn.execute"), attribute.String("outcome", "success"), attribute.String("workflow_id", "must-not-be-a-label")))
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, scope := range data.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			switch value := instrument.Data.(type) {
			case metricdata.Gauge[int64]:
				series += len(value.DataPoints)
			case metricdata.Gauge[float64]:
				series += len(value.DataPoints)
			case metricdata.Sum[int64]:
				series += len(value.DataPoints)
				for _, point := range value.DataPoints {
					if point.Attributes.HasValue("workflow_id") {
						t.Fatal("domain metric label escaped view")
					}
				}
			case metricdata.Histogram[float64]:
				for _, point := range value.DataPoints {
					series += len(point.BucketCounts) + 2
				}
			}
		}
	}
	// Classic translation: 104 histogram + 40 attempts + 26 gauges + 1 failures.
	if series != 171 {
		t.Fatalf("application series=%d, want 171 before backend metadata", series)
	}
}

func TestMalformedExporterHeadersFailWithoutDisclosingValues(t *testing.T) {
	for _, headers := range []string{"Authorization=Bearer%ZZsecret", "secret-without-equals", "bad key=secret", "Authorization=secret%0d%0aInjected:yes"} {
		t.Run(headers[:3], func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:4318/v1/metrics")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", headers)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "Authorization=valid-override")
			shutdown, err := Init(context.Background(), "test")
			if err == nil {
				_ = shutdown(context.Background())
				t.Fatal("malformed overridden generic headers accepted")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), headers) {
				t.Fatal("header leaked in initialization error")
			}
		})
	}
}

func TestFinalMetricsCollectionAndTraceFlushHaveIndependentBoundedShutdown(t *testing.T) {
	cleanEnvironment(t)
	traceReceived := make(chan struct{}, 1)
	metricsReceived := make(chan *collector.ExportMetricsServiceRequest, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			_, _ = io.Copy(io.Discard, r.Body)
			traceReceived <- struct{}{}
			w.Header().Set("Content-Type", "application/x-protobuf")
			return
		}
		body, _ := io.ReadAll(r.Body)
		request := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
		}
		metricsReceived <- request
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer backend.Close()
	defer close(release)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", backend.URL)
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "60000")
	serviceCtx, stopServices := context.WithCancel(context.Background())
	shutdown, err := Init(serviceCtx, "shutdown-test")
	if err != nil {
		t.Fatal(err)
	}
	var databaseClosed atomic.Bool
	var collections atomic.Int64
	unregister, err := RegisterStateObserver(func(ctx context.Context) (DurableState, error) {
		if databaseClosed.Load() || ctx.Err() != nil {
			t.Error("database closed or collection cancelled before final observation")
		}
		collections.Add(1)
		return DurableState{ActiveTurns: 17}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, operation := StartOperation(serviceCtx, AgentTurnExecute)
	operation.Finish(new(error))
	stopServices()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = shutdown(shutdownCtx)
	unregister()
	databaseClosed.Store(true)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("unbounded or unexpectedly successful stalled export: %v", err)
	}
	if collections.Load() != 1 {
		t.Fatalf("final snapshots = %d", collections.Load())
	}
	select {
	case <-traceReceived:
	default:
		t.Fatal("metrics stall prevented trace flush")
	}
	select {
	case request := <-metricsReceived:
		found := false
		for _, resource := range request.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, instrument := range scope.Metrics {
					if instrument.Name == "omnigrex.agent_turn.active" && instrument.GetGauge().DataPoints[0].GetAsInt() == 17 {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatal("final durable observation missing")
		}
	default:
		t.Fatal("final metrics were not exported")
	}
}
