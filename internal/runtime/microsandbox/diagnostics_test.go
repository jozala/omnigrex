package microsandbox

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"

	"github.com/jozala/omnigrex/internal/telemetry"
)

type diagnosticBuffer struct {
	mu            sync.Mutex
	content       bytes.Buffer
	notifications chan struct{}
}

func (buffer *diagnosticBuffer) Write(content []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	n, err := buffer.content.Write(content)
	select {
	case buffer.notifications <- struct{}{}:
	default:
	}
	return n, err
}
func (buffer *diagnosticBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.content.String()
}
func (buffer *diagnosticBuffer) Len() int {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.content.Len()
}
func (buffer *diagnosticBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return bytes.Clone(buffer.content.Bytes())
}

func captureDiagnostics(t *testing.T) *diagnosticBuffer {
	t.Helper()
	output := &diagnosticBuffer{notifications: make(chan struct{}, 1)}
	previous := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(output, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return output
}
