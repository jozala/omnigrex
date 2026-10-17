// Package execution defines the backend-neutral lifecycle and recovery boundary.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Key is the immutable identity of one Agent Turn execution.
// AssignmentID identifies the Participant in the backend resource contract.
type Key struct {
	AssignmentID          string
	AgentSessionID        string
	AgentTurnID           string
	ExecutionEpoch        int64
	RuntimeProfileName    string
	RuntimeProfileVersion string
}

type Ref = string

type Platform struct{ OS, Architecture string }

// Attachment names a trusted logical resource, not a host path or volume.
// The backend resolves Participant-local storage from the execution identity.
type Attachment struct{ Resource, Target string }
type TemporaryDirectory struct {
	Target     string
	SizeBytes  int64
	Executable bool
}

// Request is compiled by the agent adapter. Deployment mounts and networking
// are resolved by the selected backend, never supplied by repository code.
type Request struct {
	Key                  Key
	Workspace            WorkspaceRef
	State                StateRef
	WorkflowID           string
	Image                string
	Platform             Platform
	User                 string
	WorkingDir           string
	Command              []string
	Environment          []string
	Attachments          []Attachment
	TemporaryDirectories []TemporaryDirectory
	MemoryBytes          int64
	PIDsLimit            int64
	Requirements         []Requirement
	GoldenResources      bool
	// GoldenResourceContract is the immutable, owner-validated resource inventory,
	// not an executable script or current-deployment resource override.
	GoldenResourceContract json.RawMessage
}

type Requirement string

const (
	ProcessIsolation  Requirement = "process-isolation"
	HardwareIsolation Requirement = "hardware-isolation"
	NestedContainers  Requirement = "nested-containers"
	ToolInstallation  Requirement = "tool-installation"
)

func (Request) String() string   { return "execution request (<ephemeral configuration redacted>)" }
func (Request) GoString() string { return "execution.Request{<ephemeral configuration redacted>}" }

// Process transport closure is detachment, not evidence of termination.
type Process interface {
	Start(context.Context) error
	Transport() io.ReadWriteCloser
	ObserveExit(context.Context) (ExitObservation, error)
	Stop(context.Context, time.Duration) error
	Remove(context.Context) error
}

type Engine interface {
	Create(context.Context, Request, io.Writer) (Process, error)
	EnsureAbsent(context.Context) error
	Close() error
}
type Factory interface {
	CheckRequirements([]Requirement) error
	New(Request, Ledger) (Engine, error)
}

// ExitObservation contains positive backend-owned lifecycle evidence only.
// A zero value is unknown; EOF, exit 137 and NotFound do not establish a cause.
type ExitObservation struct {
	Resource Ref
	ExitCode int
	// ExitCodeKnown distinguishes an observed code from an unknown zero value.
	ExitCodeKnown bool
	Scope         string
	ObservedAt    time.Time
	Exited        bool
	Running       bool
	OOMKilled     bool
	// NeverStarted is positive backend evidence that this exact main resource
	// had not executed. It is distinct from exit evidence and requires dispatch
	// fencing before it can authorize preparation-only recovery cleanup.
	NeverStarted bool
}

type ManagedProcess struct {
	Identity Key
	Resource Ref
}
type DuplicateProcesses struct {
	Identity  Key
	Resources []Ref
}
type MalformedProcess struct {
	Resource Ref
	Reason   string
}
type Snapshot struct {
	Processes  []ManagedProcess
	Duplicates []DuplicateProcesses
	Malformed  []MalformedProcess
}
type Inventory interface {
	List(context.Context) (Snapshot, error)
}
type Cleaner interface {
	ObserveKeyExit(context.Context, Key) (ExitObservation, error)
	EnsureKeyAbsent(context.Context, Key) error
}

// OrphanCleaner removes only observed physical resources whose full identity
// cannot be authorized by the Store. It does not revoke a durable execution.
type OrphanCleaner interface {
	EnsureOrphansAbsent(context.Context, Key, []Ref) error
}

// ErrUnknownEffect means the external operation may still take effect.
// It must never be interpreted as a confirmed stop or absence receipt.
var ErrUnknownEffect = errors.New("execution operation has unknown effect")
var ErrInvalidSpec = errors.New("invalid Runtime Process specification")
var ErrConflict = errors.New("execution request conflicts with its immutable identity")
var ErrUnsupportedRequirement = errors.New("unsupported execution requirement")
var ErrNotFound = errors.New("execution resource not found")

// Reservation records intent before external creation. Revocation denies further
// dispatch; an unsettled reservation remains ambiguous until its exact resource
// is found or the backend records positive proof that creation had no effects.
type Reservation struct {
	Key             Key
	Digest          string
	Resource        Ref
	Revoked         bool
	CreationSettled bool
	Phase           Phase
	SetupResource   Ref
	SetupSettled    bool
}
type Phase string

const (
	Reserved Phase = "reserved"
	Setup    Phase = "setup"
	Main     Phase = "main"
)

type Ledger interface {
	ReserveExecution(context.Context, Key, string) (Reservation, bool, error)
	BeginExecutionPhase(context.Context, Key, Phase) error
	BindSetupResource(context.Context, Key, Ref) error
	RecordSetupStopped(context.Context, Key, Ref) error
	BindExecution(context.Context, Key, Ref) error
	RecordNoExecution(context.Context, Key) error
	RevokeExecution(context.Context, Key) (Reservation, bool, error)
}
