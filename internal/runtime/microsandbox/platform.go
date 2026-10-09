package microsandbox

import (
	"context"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"io"
)

// Vendor objects and SDK framing remain private to the SDK bridge.
type metadata struct {
	ID, Name, Status string
	Labels           map[string]string
}
type creation struct {
	Request                       execution.Request
	Name                          string
	Labels                        map[string]string
	Mounts                        map[string]string
	Diagnostic, DiagnosticNetwork bool
}
type event struct {
	Kind     string
	Data     []byte
	ExitCode int
}
type stream interface {
	Next(context.Context) (event, error)
	Write(context.Context, []byte) (int, error)
	Close() error
}
type platform interface {
	Create(context.Context, creation) (metadata, error)
	Lookup(context.Context, string) (metadata, error)
	List(context.Context, string) ([]metadata, string, error)
	Image(context.Context, string) (string, string, error)
	Execute(context.Context, metadata, []string, string, io.Reader) error
	Stream(context.Context, metadata, []string, string) (stream, error)
	Pause(context.Context, metadata) error
	Resume(context.Context, metadata) error
	Stop(context.Context, metadata) error
	Remove(context.Context, metadata) error
}
