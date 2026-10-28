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

// GitSnapshot is credential-free, complete Git object data and its immutable
// operation reference. Validation and transport belong to the installer.
type GitSnapshot struct {
	SHA, PackName, Ref string
	Pack, Index        []byte
}

// SnapshotInstaller performs an explicit controller-owned additive Git-data
// operation, including for read-only Reviewers. It must hold all writers
// quiescent across validation and installation and resume before returning.
// The backend acquires its execution lock before calling the supplied fence,
// and holds that fence across pause, validation, installation, and resumption.
// Errors after installation begins are ambiguous, never verification evidence.
type SnapshotInstaller interface {
	InstallSnapshot(context.Context, WorkspaceRef, GitSnapshot, WorkspaceFence) error
}

// WorkspaceFence admits an operation and excludes execution cleanup without
// blocking lease renewal. It is supplied by verified controller scope.
type WorkspaceFence func(context.Context, func(context.Context) error) error
