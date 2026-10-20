package microsandbox

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/jozala/omnigrex/internal/telemetry"
)

func captureDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&output, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}
