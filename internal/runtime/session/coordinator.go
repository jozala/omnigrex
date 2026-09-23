// Package session coordinates ACP Agent Session lifecycle preparation.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/store"
)

var (
	ErrAdditionalDirectoriesUnsupported = errors.New("ACP additional directories are unsupported")
	ErrCapabilityMismatch               = errors.New("ACP agent capabilities do not match durable session")
	ErrHumanPromptIdentityMismatch      = errors.New("human prompt does not match durable session control")
	ErrHumanPromptOutcomeUncertain      = errors.New("human prompt outcome is uncertain")
	ErrStatePathMismatch                = errors.New("runtime state path does not match Agent Session")
	ErrTurnIdentityMismatch             = errors.New("Agent Turn lease does not match durable session")
	ErrUnsupportedSessionState          = errors.New("unsupported Agent Session lifecycle state")
)

// BindingStore is the durable turn-fencing boundary needed during ACP Session preparation.
type BindingStore interface {
	ValidateTurnFence(context.Context, store.AgentTurnLease) error
	ValidateAgentSessionPromptFence(context.Context, store.AgentTurnLease, string) error
	AcquireHumanPromptLease(context.Context, string, int64, string, time.Duration) (store.HumanPromptLease, error)
	HeartbeatHumanPromptLease(context.Context, store.HumanPromptLease, time.Duration) error
	ReleaseHumanPromptLease(context.Context, store.HumanPromptLease) error
	BindAgentSessionACP(context.Context, store.AgentTurnLease, string, json.RawMessage) (store.AgentSession, error)
}

// AgentClient is the initialized-capable ACP boundary needed during Session preparation.
type AgentClient interface {
	SetAgentEventContext(agentevent.Context)
	Initialize(context.Context) (acp.InitializeResponse, error)
	RecoverCreatedSession(context.Context, string) (acp.SessionInfo, error)
	CreateSession(context.Context, acp.CreateSessionRequest) (acp.Session, error)
	ContinueSessionWithOptions(context.Context, acp.ContinueSessionRequest) ([]json.RawMessage, error)
	SetConfigOption(context.Context, string, string, any) ([]json.RawMessage, error)
}

// PromptClient is the ACP boundary used only after durable prompt authorization succeeds.
type PromptClient interface {
	SetAgentEventContext(agentevent.Context)
	Prompt(context.Context, string, []acp.ContentBlock) (acp.PromptResponse, error)
}

// HumanPromptClient is the ACP boundary for replaying and prompting under human control.
type HumanPromptClient interface {
	PromptClient
	ReplayHistory(context.Context, acp.ContinueSessionRequest) error
}

var (
	_ BindingStore      = (*store.Store)(nil)
	_ AgentClient       = (*acp.Client)(nil)
	_ PromptClient      = (*acp.Client)(nil)
	_ HumanPromptClient = (*acp.Client)(nil)
)

// Disposition describes how Prepare attached the durable Agent Session to ACP.
type Disposition string

const (
	Created   Disposition = "created"
	Recovered Disposition = "recovered"
	Continued Disposition = "continued"
)

// PrepareRequest carries an acquired Agent Turn and its corresponding durable Session.
// StatePath must identify the caller-owned assignment-isolated Runtime Process mount.
type PrepareRequest struct {
	Lease                 store.AgentTurnLease
	Session               store.AgentSession
	Client                AgentClient
	StatePath             string
	MCPServers            []acp.MCPServer
	AdditionalDirectories []string
	Configuration         opencode.SessionConfiguration
}

// Result identifies the durable and ACP Sessions attached by Prepare.
type Result struct {
	AgentSessionID string
	ACPSessionID   string
	Disposition    Disposition
	Capabilities   json.RawMessage
}

// PromptRequest authorizes one ACP prompt with the current Agent Turn and control revision.
type PromptRequest struct {
	Lease   store.AgentTurnLease
	Session store.AgentSession
	Client  PromptClient
	Content []acp.ContentBlock
}

// HumanPromptRequest carries a human-controlled Session snapshot and one prompt.
type HumanPromptRequest struct {
	Session           store.AgentSession
	Client            HumanPromptClient
	MCPServers        []acp.MCPServer
	Content           []acp.ContentBlock
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
}

const (
	defaultHumanPromptLeaseDuration     = 30 * time.Second
	defaultHumanPromptHeartbeatInterval = 10 * time.Second
	humanPromptReleaseTimeout           = 5 * time.Second
)

// Coordinator prepares ACP Sessions under an acquired Agent Turn fence.
type Coordinator struct {
	store BindingStore
}

func NewCoordinator(bindingStore BindingStore) *Coordinator {
	return &Coordinator{store: bindingStore}
}

// Prepare initializes ACP, creates or continues the Session, binds its identity, and applies configuration.
// It never submits a prompt.
func (coordinator *Coordinator) Prepare(ctx context.Context, request PrepareRequest) (Result, error) {
	if coordinator == nil || coordinator.store == nil || request.Client == nil {
		return Result{}, errors.New("prepare ACP Agent Session: dependency is nil")
	}
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	if err := coordinator.validateFence(ctx, request.Lease, "initialize ACP client"); err != nil {
		return Result{}, err
	}
	request.Client.SetAgentEventContext(agentevent.Context{})

	initialized, err := request.Client.Initialize(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("initialize ACP client: %w", err)
	}
	capabilities, err := canonicalJSON(initialized.RawAgentCapabilities)
	if err != nil {
		return Result{}, fmt.Errorf("canonicalize ACP agent capabilities: %w", err)
	}

	sessionID := request.Session.ACPSessionID
	disposition := Continued
	continueSession := request.Session.Status == store.AgentSessionActive
	var options []json.RawMessage

	switch request.Session.Status {
	case store.AgentSessionCreating:
		if err := coordinator.validateFence(ctx, request.Lease, "recover created ACP Session"); err != nil {
			return Result{}, err
		}
		recovered, recoveryErr := request.Client.RecoverCreatedSession(ctx, acp.WorkspacePath)
		switch {
		case recoveryErr == nil:
			if recovered.ID == "" {
				return Result{}, acp.ErrSessionIDEmpty
			}
			sessionID = recovered.ID
			disposition = Recovered
			continueSession = true
		case errors.Is(recoveryErr, acp.ErrSessionNotFound):
			disposition = Created
			if err := coordinator.validateFence(ctx, request.Lease, "create ACP Session"); err != nil {
				return Result{}, err
			}
			var created acp.Session
			created, err = request.Client.CreateSession(ctx, acp.CreateSessionRequest{
				CWD:                   acp.WorkspacePath,
				MCPServers:            request.MCPServers,
				AdditionalDirectories: request.AdditionalDirectories,
			})
			if err == nil {
				sessionID = created.ID
				options = created.ConfigOptions
			}
		default:
			return Result{}, fmt.Errorf("recover created ACP Session: %w", recoveryErr)
		}
		if err != nil {
			return Result{}, fmt.Errorf("attach ACP Session: %w", err)
		}
		if sessionID == "" {
			return Result{}, acp.ErrSessionIDEmpty
		}
	case store.AgentSessionActive:
		persistedCapabilities, canonicalErr := canonicalJSON(request.Session.Capabilities)
		if canonicalErr != nil || !bytes.Equal(capabilities, persistedCapabilities) {
			return Result{}, ErrCapabilityMismatch
		}
	default:
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupportedSessionState, request.Session.Status)
	}

	if err := coordinator.validateFence(ctx, request.Lease, "bind ACP Agent Session"); err != nil {
		return Result{}, err
	}
	bound, err := coordinator.store.BindAgentSessionACP(ctx, request.Lease, sessionID, capabilities)
	if err != nil {
		return Result{}, fmt.Errorf("bind ACP Agent Session: %w", err)
	}
	request.Client.SetAgentEventContext(agentEventContext(request.Lease, bound))

	if continueSession {
		if err := coordinator.validateFence(ctx, request.Lease, "continue ACP Session"); err != nil {
			return Result{}, err
		}
		options, err = request.Client.ContinueSessionWithOptions(ctx, continuationRequest(sessionID, request))
		if err != nil {
			return Result{}, fmt.Errorf("continue ACP Session: %w", err)
		}
	}

	setter := fencedConfigSetter{store: coordinator.store, lease: request.Lease, client: request.Client}
	if err := request.Configuration.Apply(ctx, setter, sessionID, options); err != nil {
		return Result{}, fmt.Errorf("configure OpenCode Session: %w", err)
	}
	return Result{
		AgentSessionID: request.Session.ID,
		ACPSessionID:   sessionID,
		Disposition:    disposition,
		Capabilities:   capabilities,
	}, nil
}

// Prompt validates the durable Agent Turn fence immediately before submitting to ACP.
func (coordinator *Coordinator) Prompt(ctx context.Context, request PromptRequest) (acp.PromptResponse, error) {
	if coordinator == nil || coordinator.store == nil || request.Client == nil {
		return acp.PromptResponse{}, errors.New("prompt ACP Agent Session: dependency is nil")
	}
	durable := request.Session
	lease := request.Lease
	if durable.Status != store.AgentSessionActive || durable.ACPSessionID == "" ||
		durable.ID != lease.AgentSessionID || durable.AgentAssignmentID != lease.AgentAssignmentID ||
		durable.ControlRevision != lease.ControlRevision || lease.JobLease.AgentSessionID != durable.ID ||
		lease.JobLease.AgentAssignmentID != durable.AgentAssignmentID || lease.JobLease.AgentTurnID != lease.ID ||
		lease.JobLease.ExecutionEpoch != lease.ExecutionEpoch {
		return acp.PromptResponse{}, ErrTurnIdentityMismatch
	}
	sessionID := durable.ACPSessionID
	if err := coordinator.store.ValidateAgentSessionPromptFence(ctx, lease, sessionID); err != nil {
		return acp.PromptResponse{}, fmt.Errorf("submit ACP prompt: %w", err)
	}
	request.Client.SetAgentEventContext(agentEventContext(lease, durable))
	response, err := request.Client.Prompt(ctx, sessionID, request.Content)
	if err != nil {
		return acp.PromptResponse{}, fmt.Errorf("submit ACP prompt: %w", err)
	}
	return response, nil
}

// HumanPrompt holds durable admission while replaying history and submitting one human prompt.
func (coordinator *Coordinator) HumanPrompt(ctx context.Context, request HumanPromptRequest) (acp.PromptResponse, error) {
	if coordinator == nil || coordinator.store == nil || request.Client == nil {
		return acp.PromptResponse{}, errors.New("prompt human-controlled ACP Agent Session: dependency is nil")
	}
	durable := request.Session
	if durable.ID == "" || durable.ACPSessionID == "" || durable.ControlRevision <= 0 ||
		durable.ControlOwner != store.SessionControlHuman ||
		durable.Status != store.AgentSessionActive && durable.Status != store.AgentSessionRetained {
		return acp.PromptResponse{}, ErrHumanPromptIdentityMismatch
	}
	leaseDuration, heartbeatInterval, err := humanPromptTiming(request)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	lease, err := coordinator.store.AcquireHumanPromptLease(ctx, durable.ID, durable.ControlRevision, durable.ACPSessionID, leaseDuration)
	if err != nil {
		return acp.PromptResponse{}, fmt.Errorf("acquire human prompt admission: %w", err)
	}

	workCtx, cancelWork := context.WithCancelCause(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := coordinator.heartbeatHumanPromptLease(heartbeatCtx, lease, leaseDuration, heartbeatInterval)
		if heartbeatErr != nil {
			cancelWork(heartbeatErr)
		}
		heartbeatDone <- heartbeatErr
	}()

	request.Client.SetAgentEventContext(agentevent.Context{})
	var response acp.PromptResponse
	promptStarted := false
	operationErr := request.Client.ReplayHistory(workCtx, acp.ContinueSessionRequest{
		SessionID:  durable.ACPSessionID,
		CWD:        acp.WorkspacePath,
		MCPServers: request.MCPServers,
	})
	if operationErr != nil {
		operationErr = fmt.Errorf("replay human-controlled ACP Session: %w", operationErr)
	}
	if operationErr == nil {
		if err := coordinator.store.HeartbeatHumanPromptLease(workCtx, lease, leaseDuration); err != nil {
			operationErr = fmt.Errorf("renew human prompt admission before submission: %w", err)
		} else {
			promptStarted = true
			response, err = request.Client.Prompt(workCtx, durable.ACPSessionID, request.Content)
			if err != nil {
				operationErr = fmt.Errorf("submit human-controlled ACP prompt: %w", err)
			}
		}
	}

	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	cancelWork(nil)
	if heartbeatErr != nil {
		operationErr = errors.Join(fmt.Errorf("heartbeat human prompt admission: %w", heartbeatErr), operationErr)
	}
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), humanPromptReleaseTimeout)
	releaseErr := coordinator.store.ReleaseHumanPromptLease(releaseCtx, lease)
	cancelRelease()
	if releaseErr != nil {
		if promptStarted {
			operationErr = errors.Join(operationErr, ErrHumanPromptOutcomeUncertain)
		}
		operationErr = errors.Join(operationErr, fmt.Errorf("release human prompt admission: %w", releaseErr))
	}
	if operationErr != nil {
		return acp.PromptResponse{}, operationErr
	}
	return response, nil
}

func humanPromptTiming(request HumanPromptRequest) (time.Duration, time.Duration, error) {
	leaseDuration := request.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = defaultHumanPromptLeaseDuration
	}
	heartbeatInterval := request.HeartbeatInterval
	if heartbeatInterval == 0 {
		heartbeatInterval = defaultHumanPromptHeartbeatInterval
	}
	const maximumDuration = 365 * 24 * time.Hour
	if leaseDuration < time.Microsecond || leaseDuration > maximumDuration ||
		heartbeatInterval < time.Microsecond || heartbeatInterval > maximumDuration || heartbeatInterval >= leaseDuration {
		return 0, 0, errors.New("human prompt heartbeat must be positive and shorter than its lease")
	}
	return leaseDuration, heartbeatInterval, nil
}

func (coordinator *Coordinator) heartbeatHumanPromptLease(ctx context.Context, lease store.HumanPromptLease, extension, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := coordinator.store.HeartbeatHumanPromptLease(ctx, lease, extension); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func agentEventContext(lease store.AgentTurnLease, durable store.AgentSession) agentevent.Context {
	return agentevent.Context{
		AssignmentID:    durable.AgentAssignmentID,
		AgentSessionID:  durable.ID,
		ACPSessionID:    durable.ACPSessionID,
		TurnID:          lease.ID,
		ExecutionEpoch:  uint64(lease.ExecutionEpoch),
		ControlRevision: uint64(lease.ControlRevision),
	}
}

func validateRequest(request PrepareRequest) error {
	if request.StatePath == "" || request.StatePath != request.Session.RuntimeStatePath {
		return ErrStatePathMismatch
	}
	if len(request.AdditionalDirectories) != 0 {
		return ErrAdditionalDirectoriesUnsupported
	}
	lease := request.Lease
	durable := request.Session
	if durable.ID == "" || durable.ID != lease.AgentSessionID ||
		durable.AgentAssignmentID == "" || durable.AgentAssignmentID != lease.AgentAssignmentID ||
		durable.ControlRevision <= 0 || durable.ControlRevision != lease.ControlRevision ||
		lease.ExecutionEpoch <= 0 || durable.NextExecutionEpoch <= 1 || durable.NextExecutionEpoch-1 != lease.ExecutionEpoch ||
		lease.JobLease.AgentSessionID != lease.AgentSessionID ||
		lease.JobLease.AgentAssignmentID != lease.AgentAssignmentID ||
		lease.JobLease.AgentTurnID == "" || lease.JobLease.AgentTurnID != lease.ID ||
		lease.JobLease.ExecutionEpoch != lease.ExecutionEpoch {
		return ErrTurnIdentityMismatch
	}
	switch request.Session.Status {
	case store.AgentSessionCreating:
		if request.Session.ACPSessionID != "" {
			return fmt.Errorf("%w: CREATING Session has an ACP identity", ErrUnsupportedSessionState)
		}
	case store.AgentSessionActive:
		if request.Session.ACPSessionID == "" {
			return fmt.Errorf("%w: ACTIVE Session has no ACP identity", ErrUnsupportedSessionState)
		}
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedSessionState, request.Session.Status)
	}
	return nil
}

func (coordinator *Coordinator) validateFence(ctx context.Context, lease store.AgentTurnLease, operation string) error {
	if err := coordinator.store.ValidateTurnFence(ctx, lease); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

type fencedConfigSetter struct {
	store  BindingStore
	lease  store.AgentTurnLease
	client AgentClient
}

func (setter fencedConfigSetter) SetConfigOption(ctx context.Context, sessionID, configID string, value any) ([]json.RawMessage, error) {
	if err := setter.store.ValidateTurnFence(ctx, setter.lease); err != nil {
		return nil, fmt.Errorf("validate Agent Turn fence before OpenCode Session %s option: %w", configID, err)
	}
	return setter.client.SetConfigOption(ctx, sessionID, configID, value)
}

func continuationRequest(sessionID string, request PrepareRequest) acp.ContinueSessionRequest {
	return acp.ContinueSessionRequest{
		SessionID:             sessionID,
		CWD:                   acp.WorkspacePath,
		MCPServers:            request.MCPServers,
		AdditionalDirectories: request.AdditionalDirectories,
	}
}

func canonicalJSON(value json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(value) {
		return nil, errors.New("not valid JSON")
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(decoded)
	return json.RawMessage(canonical), err
}
