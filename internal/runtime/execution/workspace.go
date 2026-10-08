package execution

import (
	"context"
	"io"
)

// WorkspaceRef identifies the prepared checkout for one exact execution.
// Locations and backend resource names are private to its owner.
type WorkspaceRef struct {
	Key  Key
	Seed string
}

type CapturePurpose string

const (
	PublicationCapture CapturePurpose = "publication"
	TerminalCapture    CapturePurpose = "terminal"
)

type CaptureRequest struct {
	Workspace   WorkspaceRef
	OperationID string
	Purpose     CapturePurpose
}

type CaptureRef struct {
	Request CaptureRequest
	SHA256  string
}

// WorkspaceSource holds every writer quiescent for the complete callback.
// The callback receives a complete tar with guest-visible modes and symlinks,
// not a live host path or raw vendor filesystem metadata.
// Successful terminal capture additionally requires confirmed environment stop.
// Failure to resume a suspended environment is an operation failure.
type WorkspaceSource interface {
	ReadWorkspace(context.Context, CaptureRequest, func(io.Reader) error) error
}

type WorkspaceCaptures interface {
	Capture(context.Context, CaptureRequest) (CaptureRef, error)
	OpenCapture(context.Context, CaptureRef) (io.ReadCloser, error)
}

// CaptureLedger retains immutable sealed identity separately from artifact
// storage, so losing bytes cannot authorize sealing a later observation.
// Recording these facts never grants capture, prompting or settlement authority.
type CaptureLedger interface {
	LoadCapture(context.Context, CaptureRequest) (CaptureRef, bool, error)
	SealCapture(context.Context, CaptureRef) error
}
