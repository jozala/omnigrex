package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/jozala/omnigrex/internal/uuidtext"
)

var ErrBindingMismatch = errors.New("retained state backend or contract mismatch")
var ErrStateUnavailable = errors.New("protected retained state unavailable")
var ErrCorruptState = errors.New("corrupt retained state archive")

// BackendBinding identifies a storage/inventory namespace, not an SDK version.
type BackendBinding struct {
	Family    string `json:"family"`
	Version   int    `json:"version"`
	Namespace string `json:"namespace"`
}

// StateRef identifies retained data independently of any execution environment.
// A Participant can own multiple Sessions in its transitional OpenCode store.
// SessionID scopes attachment authority; collection/export use the Participant root.
type StateRef struct {
	ID                   string         `json:"id"`
	ParticipantID        string         `json:"participant_id"`
	SessionID            string         `json:"session_id,omitempty"`
	Backend              BackendBinding `json:"backend"`
	Format               string         `json:"format"`
	RuntimeProfileSHA256 string         `json:"runtime_profile_sha256"`
	Image                string         `json:"image"`
}

func (ref StateRef) Validate() error {
	if !uuidtext.ValidCanonicalNonNil(ref.ID) || ref.ID != ref.ParticipantID ||
		(ref.SessionID != "" && !uuidtext.ValidCanonicalNonNil(ref.SessionID)) ||
		ref.Backend.Family == "" || ref.Backend.Version <= 0 || ref.Backend.Namespace == "" || ref.Format == "" || ref.Image == "" {
		return ErrBindingMismatch
	}
	return nil
}

// StateMetadataSource supplies protected, versioned adapter-owned metadata.
// A reference alone never authorizes writing, collection or recovery.
type StateMetadataSource interface {
	StateMetadata(context.Context, StateRef) (json.RawMessage, bool, error)
}

type RetainedState interface {
	Ensure(context.Context, StateRef) error
	Inspect(context.Context, StateRef) error
	Collect(context.Context, StateRef) error
}

// StateRecovery requires a stopped-writer boundary established by its caller.
// Export includes only Session data; a coordinated recovery set also needs the
// database and protected Workspace Captures.
type StateRecovery interface {
	Export(context.Context, StateRef, io.Writer) error
	Restore(context.Context, StateRef, io.Reader) error
}
