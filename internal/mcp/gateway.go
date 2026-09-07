package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	ProtocolVersion                    = "2025-11-25"
	defaultMaxRequestSize              = int64(1 << 20)
	defaultMutationFenceCheckInterval  = time.Second
	defaultMutationFinalizationTimeout = 10 * time.Second
	defaultMutationOperationTimeout    = 2 * time.Hour
	maximumMutationFenceCheckInterval  = 365 * 24 * time.Hour
	maximumMutationFinalizationTimeout = 365 * 24 * time.Hour
	maximumMutationOperationTimeout    = 365 * 24 * time.Hour
	tokenBytes                         = 32
)

var (
	ErrInvalidConfiguration = errors.New("invalid MCP gateway configuration")
	// ErrMutationDrainUnresolved indicates that at least one admitted mutation did not reach a durable terminal or recoverable state.
	ErrMutationDrainUnresolved = errors.New("admitted mutation durable state is unresolved")
)

// Store is the durable fencing and mutation ledger needed by the gateway.
type Store interface {
	ValidateTurnFence(context.Context, store.AgentTurnLease) error
	ReserveMutation(context.Context, store.AgentTurnLease, store.MutationSpec) (store.MutationReservation, error)
	AcknowledgeMutationReplay(context.Context, store.AgentTurnLease, string, store.MutationSpec) error
	StartMutation(context.Context, store.AgentTurnLease, string) (store.MutationReservation, error)
	CompleteMutation(context.Context, store.AgentTurnLease, string, json.RawMessage) error
	FailMutation(context.Context, store.AgentTurnLease, string, error) error
	MarkMutationUnknown(context.Context, store.AgentTurnLease, string, error) error
}

// Backend performs one authorized tool operation. It never receives the bearer token or lease credentials.
type Backend interface {
	Execute(context.Context, Invocation) (json.RawMessage, error)
}

// MutationPlanner supplies backend-owned reservation metadata before a mutation is admitted.
type MutationPlanner interface {
	PlanMutation(context.Context, Invocation) (MutationMetadata, error)
}

// MutationReplayRestorer validates a successful ancestor result and restores backend turn state without repeating its side effect.
type MutationReplayRestorer interface {
	RestoreMutationReplay(context.Context, Invocation, store.MutationReservation) error
}

// TurnReleaser optionally retires backend state once a registration is closed and drained.
type TurnReleaser interface {
	ReleaseTurn(ToolScope)
}

// ReadLedger optionally records read-tool outcomes without coupling the gateway to a store implementation.
type ReadLedger interface {
	RecordRead(context.Context, store.AgentTurnLease, ReadRecord) error
}

type Config struct {
	EndpointURL                 string
	Store                       Store
	Backend                     Backend
	Ledger                      ReadLedger
	LifecycleContext            context.Context
	MutationFenceCheckInterval  time.Duration
	MutationFinalizationTimeout time.Duration
	MutationOperationTimeout    time.Duration
	MaxRequestBytes             int64
	Now                         func() time.Time
	Random                      io.Reader
}

type RepositoryScope struct {
	ID    int64  `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type IssueScope struct {
	ID     int64 `json:"id"`
	Number int64 `json:"number"`
}

type PullRequestScope struct {
	ID     int64 `json:"id"`
	Number int64 `json:"number"`
}

// TokenScope is the complete authority captured by one per-turn token.
type TokenScope struct {
	Lease         store.AgentTurnLease
	WorkflowID    string
	Role          workflow.Role
	Repository    RepositoryScope
	Issue         IssueScope
	PullRequest   *PullRequestScope
	Branch        string
	DefaultBranch string
	HeadSHA       string
	AllowedTools  []string
	ExpiresAt     time.Time
}

// ToolScope is credential-free context passed to the backend.
type ToolScope struct {
	WorkflowID        string            `json:"workflow_id"`
	AgentAssignmentID string            `json:"agent_assignment_id"`
	AgentSessionID    string            `json:"agent_session_id"`
	AgentTurnID       string            `json:"agent_turn_id"`
	ExecutionEpoch    int64             `json:"execution_epoch"`
	Role              workflow.Role     `json:"role"`
	Repository        RepositoryScope   `json:"repository"`
	Issue             IssueScope        `json:"issue"`
	PullRequest       *PullRequestScope `json:"pull_request,omitempty"`
	Branch            string            `json:"branch"`
	DefaultBranch     string            `json:"default_branch"`
	HeadSHA           string            `json:"head_sha"`
	TurnCreatedAt     time.Time         `json:"turn_created_at"`
}

type Invocation struct {
	Name        string
	Arguments   json.RawMessage
	Scope       ToolScope
	Class       ToolClass
	OperationID string
	Mutation    MutationMetadata
}

type MutationMetadata struct {
	ExternalService    string
	ExternalResourceID string
	ExpectedSHA        string
}

type unknownOutcomeError struct{ cause error }

func (err unknownOutcomeError) Error() string { return "mutation outcome is unknown" }
func (err unknownOutcomeError) Unwrap() error { return err.cause }

// OutcomeUnknown classifies a backend failure for which the external side effect may have happened.
func OutcomeUnknown(cause error) error {
	if cause == nil {
		cause = errors.New("unspecified ambiguous backend failure")
	}
	return unknownOutcomeError{cause: cause}
}

// IsOutcomeUnknown reports whether an error represents an ambiguous external side effect.
func IsOutcomeUnknown(err error) bool {
	var unknown unknownOutcomeError
	return errors.As(err, &unknown)
}

type ReadRecord struct {
	Invocation Invocation
	Result     json.RawMessage
	LastError  string
	StartedAt  time.Time
	FinishedAt time.Time
	Succeeded  bool
}

// Registration is a revocable handle plus the remote MCP descriptor passed through ACP.
// Its internal identity is deliberately not the bearer credential.
type Registration struct {
	Server  acp.MCPServer
	id      uint64
	gateway *Gateway
	grant   *grant
}

func (registration Registration) String() string {
	return "MCP registration"
}

func (registration Registration) GoString() string {
	return "mcp.Registration{Server:<redacted>}"
}

type Gateway struct {
	endpointURL                 string
	store                       Store
	backend                     Backend
	ledger                      ReadLedger
	maxBody                     int64
	now                         func() time.Time
	random                      io.Reader
	lifecycle                   context.Context
	mutationFenceCheckInterval  time.Duration
	mutationFinalizationTimeout time.Duration
	mutationOperationTimeout    time.Duration

	mutex         sync.RWMutex
	randomMutex   sync.Mutex
	nextID        uint64
	registrations map[[sha256.Size]byte]*grant
	gates         map[string]*mutationGate
}

type mutationGate struct {
	channel chan struct{}
	refs    int
}

type grant struct {
	id        uint64
	tokenHash [sha256.Size]byte
	scope     TokenScope

	mutex        sync.Mutex
	initializing bool
	initialized  bool
	closed       bool
	admitted     int
	unresolved   bool
	drained      chan struct{}
	gate         *mutationGate
	operationCtx context.Context
	cancelOps    context.CancelFunc
	retireWait   sync.Once
	retireOnce   sync.Once
}

func New(config Config) (*Gateway, error) {
	endpoint, err := url.Parse(config.EndpointURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: endpoint URL", ErrInvalidConfiguration)
	}
	if config.Store == nil || config.Backend == nil {
		return nil, fmt.Errorf("%w: Store and Backend are required", ErrInvalidConfiguration)
	}
	if config.MaxRequestBytes < 0 {
		return nil, fmt.Errorf("%w: request size limit", ErrInvalidConfiguration)
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = defaultMaxRequestSize
	}
	if config.MutationFenceCheckInterval == 0 {
		config.MutationFenceCheckInterval = defaultMutationFenceCheckInterval
	}
	if config.MutationFenceCheckInterval < time.Microsecond || config.MutationFenceCheckInterval > maximumMutationFenceCheckInterval {
		return nil, fmt.Errorf("%w: mutation fence check interval", ErrInvalidConfiguration)
	}
	if config.MutationFinalizationTimeout == 0 {
		config.MutationFinalizationTimeout = defaultMutationFinalizationTimeout
	}
	if config.MutationFinalizationTimeout < time.Microsecond || config.MutationFinalizationTimeout > maximumMutationFinalizationTimeout {
		return nil, fmt.Errorf("%w: mutation finalization timeout", ErrInvalidConfiguration)
	}
	if config.MutationOperationTimeout == 0 {
		config.MutationOperationTimeout = defaultMutationOperationTimeout
	}
	if config.MutationOperationTimeout < time.Microsecond || config.MutationOperationTimeout > maximumMutationOperationTimeout {
		return nil, fmt.Errorf("%w: mutation operation timeout", ErrInvalidConfiguration)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.LifecycleContext == nil {
		config.LifecycleContext = context.Background()
	}
	return &Gateway{
		endpointURL:                 config.EndpointURL,
		store:                       config.Store,
		backend:                     config.Backend,
		ledger:                      config.Ledger,
		maxBody:                     config.MaxRequestBytes,
		now:                         config.Now,
		random:                      config.Random,
		lifecycle:                   config.LifecycleContext,
		mutationFenceCheckInterval:  config.MutationFenceCheckInterval,
		mutationFinalizationTimeout: config.MutationFinalizationTimeout,
		mutationOperationTimeout:    config.MutationOperationTimeout,
		registrations:               make(map[[sha256.Size]byte]*grant),
		gates:                       make(map[string]*mutationGate),
	}, nil
}

func (gateway *Gateway) Register(scope TokenScope) (Registration, error) {
	if err := validateTokenScope(scope); err != nil {
		return Registration{}, err
	}
	now := gateway.now()
	if !now.Before(scope.ExpiresAt) || !now.Before(scope.Lease.LeaseExpiresAt) {
		return Registration{}, fmt.Errorf("%w: expired token scope", ErrInvalidConfiguration)
	}
	tools, err := toolsForScope(scope)
	if err != nil {
		return Registration{}, err
	}
	scope.AllowedTools = make([]string, len(tools))
	for index, tool := range tools {
		scope.AllowedTools[index] = tool.Name
	}
	var token string
	var hash [sha256.Size]byte
	var id uint64
	var registeredGrant *grant
	for {
		raw := make([]byte, tokenBytes)
		gateway.randomMutex.Lock()
		_, err = io.ReadFull(gateway.random, raw)
		gateway.randomMutex.Unlock()
		if err != nil {
			return Registration{}, fmt.Errorf("register MCP token: random source failed")
		}
		token = base64.RawURLEncoding.EncodeToString(raw)
		hash = sha256.Sum256([]byte(token))
		gateway.mutex.Lock()
		if gateway.registrations[hash] == nil {
			gateway.nextID++
			id = gateway.nextID
			gate := gateway.gates[scope.Lease.AgentSessionID]
			if gate == nil {
				gate = &mutationGate{channel: make(chan struct{}, 1)}
				gateway.gates[scope.Lease.AgentSessionID] = gate
			}
			gate.refs++
			operationCtx, cancelOps := context.WithCancel(gateway.lifecycle)
			registeredGrant = &grant{
				id: id, tokenHash: hash, scope: cloneScope(scope), drained: make(chan struct{}), gate: gate,
				operationCtx: operationCtx, cancelOps: cancelOps,
			}
			gateway.registrations[hash] = registeredGrant
			gateway.mutex.Unlock()
			break
		}
		gateway.mutex.Unlock()
	}

	return Registration{
		id: id, gateway: gateway, grant: registeredGrant,
		Server: acp.MCPServer{
			Type: "http", Name: ServerName, URL: gateway.endpointURL,
			Headers: []acp.EnvironmentEntry{{Name: "Authorization", Value: "Bearer " + token}},
		},
	}, nil
}

// CloseAndDrain revokes a registration and waits for its admitted mutations to reach a durable terminal or recoverable state.
func (gateway *Gateway) CloseAndDrain(ctx context.Context, registration Registration) error {
	if registration.gateway != gateway || registration.id == 0 || registration.grant == nil || registration.grant.id != registration.id {
		return fmt.Errorf("%w: registration", ErrInvalidConfiguration)
	}

	drained := gateway.closeGrant(registration.grant, true)

	select {
	case <-drained:
		gateway.retireGrant(registration.grant)
		return registration.grant.drainResult()
	default:
	}
	select {
	case <-drained:
		gateway.retireGrant(registration.grant)
		return registration.grant.drainResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Revoke removes a token immediately. It does not cancel mutations that were already admitted.
func (gateway *Gateway) Revoke(registration Registration) bool {
	if registration.gateway != gateway || registration.id == 0 || registration.grant == nil || registration.grant.id != registration.id {
		return false
	}
	gateway.mutex.Lock()
	removed := gateway.registrations[registration.grant.tokenHash] == registration.grant
	if removed {
		delete(gateway.registrations, registration.grant.tokenHash)
	}
	gateway.mutex.Unlock()
	if !removed {
		return false
	}
	gateway.closeGrant(registration.grant, false)
	return true
}

func (gateway *Gateway) closeGrant(registration *grant, cancelOperations bool) <-chan struct{} {
	gateway.mutex.Lock()
	if gateway.registrations[registration.tokenHash] == registration {
		delete(gateway.registrations, registration.tokenHash)
	}
	gateway.mutex.Unlock()

	registration.mutex.Lock()
	if !registration.closed {
		registration.closed = true
		if registration.admitted == 0 {
			close(registration.drained)
		}
	}
	if cancelOperations {
		registration.cancelOps()
	}
	drained := registration.drained
	registration.mutex.Unlock()
	select {
	case <-drained:
		gateway.retireGrant(registration)
	default:
		registration.retireWait.Do(func() {
			go func() {
				<-drained
				gateway.retireGrant(registration)
			}()
		})
	}
	return drained
}

func (gateway *Gateway) retireGrant(registration *grant) {
	registration.retireOnce.Do(func() {
		if releaser, ok := gateway.backend.(TurnReleaser); ok {
			releaser.ReleaseTurn(backendScope(registration.scope))
		}
		gateway.mutex.Lock()
		defer gateway.mutex.Unlock()
		registration.gate.refs--
		if registration.gate.refs == 0 && gateway.gates[registration.scope.Lease.AgentSessionID] == registration.gate {
			delete(gateway.gates, registration.scope.Lease.AgentSessionID)
		}
	})
}

func validateTokenScope(scope TokenScope) error {
	lease := scope.Lease
	var profile struct {
		Role workflow.Role `json:"role"`
	}
	if json.Unmarshal(lease.AgentProfileConfig, &profile) != nil || profile.Role != scope.Role {
		return fmt.Errorf("%w: Role does not match Agent Turn", ErrInvalidConfiguration)
	}
	if scope.WorkflowID == "" || scope.WorkflowID != lease.JobLease.WorkflowID ||
		lease.ID == "" || lease.JobLease.AgentTurnID != lease.ID || lease.AgentSessionID == "" ||
		lease.JobLease.AgentSessionID != lease.AgentSessionID || lease.AgentAssignmentID == "" ||
		lease.JobLease.AgentAssignmentID != lease.AgentAssignmentID || lease.ExecutionEpoch <= 0 ||
		lease.JobLease.ExecutionEpoch != lease.ExecutionEpoch || lease.OwnerID == "" || lease.OwnerToken == "" ||
		scope.Repository.ID <= 0 || strings.TrimSpace(scope.Repository.Owner) == "" || strings.TrimSpace(scope.Repository.Name) == "" ||
		scope.Issue.ID <= 0 || scope.Issue.Number <= 0 || scope.ExpiresAt.IsZero() ||
		lease.LeaseExpiresAt.IsZero() || scope.ExpiresAt.After(lease.LeaseExpiresAt) ||
		(scope.Role != workflow.RoleDeveloper && scope.Role != workflow.RoleReviewer) {
		return fmt.Errorf("%w: token scope", ErrInvalidConfiguration)
	}
	if scope.PullRequest != nil && (scope.PullRequest.ID <= 0 || scope.PullRequest.Number <= 0) {
		return fmt.Errorf("%w: Pull Request scope", ErrInvalidConfiguration)
	}
	if scope.Role == workflow.RoleReviewer && scope.PullRequest == nil {
		return fmt.Errorf("%w: Reviewer requires Pull Request scope", ErrInvalidConfiguration)
	}
	if scope.Role == workflow.RoleReviewer && (lease.ChangeProposalID == "" || lease.ExpectedHeadSHA == "") {
		return fmt.Errorf("%w: Reviewer Agent Turn has no Change Proposal", ErrInvalidConfiguration)
	}
	if !validBranchResourceName(scope.Branch) || !validBranchResourceName(scope.DefaultBranch) || strings.TrimSpace(scope.HeadSHA) == "" {
		return fmt.Errorf("%w: repository revision scope", ErrInvalidConfiguration)
	}
	if lease.CreatedAt.IsZero() {
		return fmt.Errorf("%w: Agent Turn creation time", ErrInvalidConfiguration)
	}
	if lease.ExpectedHeadSHA != "" && lease.ExpectedHeadSHA != scope.HeadSHA {
		return fmt.Errorf("%w: head does not match Agent Turn", ErrInvalidConfiguration)
	}
	if lease.ChangeProposalID != "" && scope.PullRequest == nil {
		return fmt.Errorf("%w: missing Pull Request scope", ErrInvalidConfiguration)
	}
	return nil
}

func cloneScope(scope TokenScope) TokenScope {
	scope.Lease.AgentProfileContentSHA256 = append([]byte(nil), scope.Lease.AgentProfileContentSHA256...)
	scope.Lease.AgentProfileConfig = append(json.RawMessage(nil), scope.Lease.AgentProfileConfig...)
	scope.Lease.JobLease.Payload = append(json.RawMessage(nil), scope.Lease.JobLease.Payload...)
	scope.Lease.JobLease.Result = append(json.RawMessage(nil), scope.Lease.JobLease.Result...)
	scope.Lease.JobLease.LeasedAt = cloneTime(scope.Lease.JobLease.LeasedAt)
	scope.Lease.JobLease.LeaseExpiresAt = cloneTime(scope.Lease.JobLease.LeaseExpiresAt)
	scope.Lease.JobLease.HeartbeatAt = cloneTime(scope.Lease.JobLease.HeartbeatAt)
	scope.Lease.JobLease.CompletedAt = cloneTime(scope.Lease.JobLease.CompletedAt)
	scope.AllowedTools = append([]string(nil), scope.AllowedTools...)
	if scope.PullRequest != nil {
		pullRequest := *scope.PullRequest
		scope.PullRequest = &pullRequest
	}
	return scope
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func (gateway *Gateway) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if len(request.Header.Values("Origin")) != 0 {
		http.Error(response, "Origin is not allowed", http.StatusForbidden)
		return
	}
	registration, ok := gateway.authenticate(request)
	if !ok {
		response.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := gateway.store.ValidateTurnFence(request.Context(), registration.scope.Lease); err != nil {
		response.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !gateway.grantLive(registration) {
		response.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if request.URL.Path != parsedPath(gateway.endpointURL) {
		http.NotFound(response, request)
		return
	}
	switch request.Method {
	case http.MethodGet:
		if !ready(request, registration) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		if !accepts(request.Header.Get("Accept"), "text/event-stream") {
			http.Error(response, "Accept must include text/event-stream", http.StatusNotAcceptable)
			return
		}
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		if !ready(request, registration) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	case http.MethodPost:
	default:
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !accepts(request.Header.Get("Accept"), "application/json") || !accepts(request.Header.Get("Accept"), "text/event-stream") {
		http.Error(response, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	if !hasMediaType(request.Header.Get("Content-Type"), "application/json") {
		http.Error(response, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	request.Body = http.MaxBytesReader(response, request.Body, gateway.maxBody)
	decoder := json.NewDecoder(request.Body)
	var rpc rpcRequest
	if err := decoder.Decode(&rpc); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(response, "invalid JSON-RPC request", status)
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF || rpc.JSONRPC != "2.0" || rpc.Method == "" {
		http.Error(response, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	if len(rpc.ID) != 0 && !validRequestID(rpc.ID) {
		writeRPCError(response, json.RawMessage("null"), -32600, "invalid request")
		return
	}

	switch rpc.Method {
	case "initialize":
		gateway.initialize(response, request, registration, rpc)
	case "notifications/initialized":
		gateway.completeInitialization(response, request, registration, rpc)
	case "tools/list":
		gateway.listTools(response, request, registration, rpc)
	case "tools/call":
		gateway.callTool(response, request, registration, rpc)
	default:
		if !ready(request, registration) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		if len(rpc.ID) == 0 {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPCError(response, rpc.ID, -32601, "method not found")
	}
}

func (gateway *Gateway) callTool(response http.ResponseWriter, request *http.Request, registration *grant, rpc rpcRequest) {
	if !ready(request, registration) || len(rpc.ID) == 0 {
		http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
		return
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Metadata  json.RawMessage `json:"_meta"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(rpc.Params)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&params) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!validRequestMetadata(params.Metadata) || !slicesContains(registration.scope.AllowedTools, params.Name) {
		writeRPCError(response, rpc.ID, -32602, "invalid tool call")
		return
	}
	tool, found := definition(params.Name)
	if !found || validateArguments(params.Arguments, tool.InputSchema) != nil {
		writeRPCError(response, rpc.ID, -32602, "invalid tool arguments")
		return
	}
	if !scopeAllowsTool(registration.scope, params.Name) {
		writeToolError(response, rpc.ID, "tool is unavailable in this turn")
		return
	}
	canonicalArguments, err := canonicalJSON(params.Arguments)
	if err != nil {
		writeRPCError(response, rpc.ID, -32602, "invalid tool arguments")
		return
	}
	invocation := Invocation{
		Name: params.Name, Arguments: append(json.RawMessage(nil), canonicalArguments...),
		Scope: backendScope(registration.scope), Class: tool.Class,
	}
	if tool.Class == MutationTool {
		gateway.callMutation(response, request, registration, rpc.ID, invocation)
		return
	}
	if !registration.admit() {
		writeRPCError(response, rpc.ID, -32001, "tool authorization is stale")
		return
	}
	defer registration.finishCall(true)
	started := gateway.now()
	recordedInvocation := cloneInvocation(invocation)
	result, err := gateway.backend.Execute(request.Context(), invocation)
	succeeded := err == nil && len(result) != 0 && json.Valid(result)
	if gateway.ledger != nil {
		record := ReadRecord{
			Invocation: recordedInvocation, StartedAt: started, FinishedAt: gateway.now(), Succeeded: succeeded,
		}
		if succeeded {
			record.Result = append(json.RawMessage(nil), result...)
		} else {
			record.LastError = "read tool failed"
		}
		if err := gateway.ledger.RecordRead(context.WithoutCancel(request.Context()), registration.scope.Lease, record); err != nil {
			writeToolError(response, rpc.ID, "tool call could not be recorded")
			return
		}
	}
	if !succeeded {
		writeToolError(response, rpc.ID, "tool call failed")
		return
	}
	writeToolResult(response, rpc.ID, result)
}

func validRequestMetadata(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var metadata map[string]json.RawMessage
	return json.Unmarshal(raw, &metadata) == nil && metadata != nil
}

func scopeAllowsTool(scope TokenScope, tool string) bool {
	switch tool {
	case ToolGetPullRequest, ToolListPullRequestReviews, ToolListReviewThreads,
		ToolSubmitReview, ToolCommentOnPullRequest:
		return scope.PullRequest != nil
	case ToolOpenPR:
		return scope.PullRequest == nil
	default:
		return true
	}
}

func cloneInvocation(invocation Invocation) Invocation {
	invocation.Arguments = append(json.RawMessage(nil), invocation.Arguments...)
	if invocation.Scope.PullRequest != nil {
		pullRequest := *invocation.Scope.PullRequest
		invocation.Scope.PullRequest = &pullRequest
	}
	return invocation
}

type mutationOutcome struct {
	result          json.RawMessage
	message         string
	durablyResolved bool
}

func (gateway *Gateway) callMutation(response http.ResponseWriter, request *http.Request, registration *grant, id json.RawMessage, invocation Invocation) {
	operationID, err := requiredStringArgument(invocation.Arguments, "operation_id")
	if err != nil {
		writeRPCError(response, id, -32602, "invalid tool arguments")
		return
	}
	invocation.OperationID = operationID
	invocation.Mutation = mutationMetadata(invocation.Name, registration.scope)
	gate := registration.gate.channel
	select {
	case gate <- struct{}{}:
	case <-request.Context().Done():
		return
	}

	if err := gateway.store.ValidateTurnFence(request.Context(), registration.scope.Lease); err != nil {
		<-gate
		writeRPCError(response, id, -32001, "tool authorization is stale")
		return
	}
	if !gateway.grantLive(registration) {
		<-gate
		writeRPCError(response, id, -32001, "tool authorization is stale")
		return
	}
	if !registration.admit() {
		<-gate
		writeRPCError(response, id, -32001, "tool authorization is stale")
		return
	}
	operationContext, cancelOperation := context.WithTimeout(registration.operationCtx, gateway.mutationOperationTimeout)
	finishCall := func(durablyResolved bool) {
		cancelOperation()
		registration.finishCall(durablyResolved)
		<-gate
	}
	if planner, ok := gateway.backend.(MutationPlanner); ok {
		invocation.Mutation, err = planner.PlanMutation(operationContext, invocation)
		if err != nil {
			finishCall(true)
			writeToolError(response, id, "mutation planning failed")
			return
		}
	}
	spec := store.MutationSpec{
		OperationID: operationID, ToolName: invocation.Name, Request: append(json.RawMessage(nil), invocation.Arguments...),
		ExternalService: invocation.Mutation.ExternalService, ExternalResourceID: invocation.Mutation.ExternalResourceID,
		ExpectedSHA: invocation.Mutation.ExpectedSHA,
	}
	reservation, err := gateway.store.ReserveMutation(operationContext, registration.scope.Lease, spec)
	if err != nil {
		finishCall(false)
		switch {
		case errors.Is(err, store.ErrMutationOperationConflict):
			writeToolError(response, id, "mutation operation identity conflict")
		case errors.Is(err, store.ErrMutationAdmissionClosed):
			writeToolError(response, id, "mutation admission is closed")
		default:
			writeToolError(response, id, "mutation was not admitted")
		}
		return
	}
	switch reservation.State {
	case store.MutationSucceeded:
		if reservation.AgentTurnID != registration.scope.Lease.ID || reservation.ExecutionEpoch != registration.scope.Lease.ExecutionEpoch {
			restorer, ok := gateway.backend.(MutationReplayRestorer)
			if !ok {
				finishCall(true)
				writeToolError(response, id, "cached mutation replay failed")
				return
			}
			replayInvocation := cloneInvocation(invocation)
			replayInvocation.OperationID = reservation.ID
			replayInvocation.Mutation = MutationMetadata{
				ExternalService: reservation.ExternalService, ExternalResourceID: reservation.ExternalResourceID,
				ExpectedSHA: reservation.ExpectedSHA,
			}
			if err := restorer.RestoreMutationReplay(operationContext, replayInvocation, reservation); err != nil ||
				gateway.store.AcknowledgeMutationReplay(operationContext, registration.scope.Lease, reservation.ID, spec) != nil {
				finishCall(true)
				writeToolError(response, id, "cached mutation replay failed")
				return
			}
		}
		finishCall(true)
		if len(reservation.Result) == 0 || !json.Valid(reservation.Result) {
			writeToolError(response, id, "cached mutation result is unavailable")
			return
		}
		writeToolResult(response, id, reservation.Result)
		return
	case store.MutationFailed:
		if (reservation.AgentTurnID != registration.scope.Lease.ID || reservation.ExecutionEpoch != registration.scope.Lease.ExecutionEpoch) &&
			gateway.store.AcknowledgeMutationReplay(operationContext, registration.scope.Lease, reservation.ID, spec) != nil {
			finishCall(true)
			writeToolError(response, id, "cached mutation replay failed")
			return
		}
		finishCall(true)
		writeToolError(response, id, "mutation previously failed")
		return
	case store.MutationUnknown, store.MutationReconciling, store.MutationInFlight:
		finishCall(true)
		writeToolError(response, id, "mutation outcome is unresolved")
		return
	case store.MutationReserved:
		invocation.OperationID = reservation.ID
	default:
		finishCall(true)
		writeToolError(response, id, "mutation state is invalid")
		return
	}

	outcome := make(chan mutationOutcome, 1)
	go func() {
		defer func() { <-gate }()
		defer cancelOperation()
		completed := gateway.executeMutation(operationContext, registration.scope.Lease, reservation, invocation)
		registration.finishCall(completed.durablyResolved)
		outcome <- completed
	}()
	select {
	case completed := <-outcome:
		if completed.message != "" {
			writeToolError(response, id, completed.message)
			return
		}
		writeToolResult(response, id, completed.result)
	case <-request.Context().Done():
		return
	}
}

func (registration *grant) admit() bool {
	registration.mutex.Lock()
	defer registration.mutex.Unlock()
	if registration.closed {
		return false
	}
	registration.admitted++
	return true
}

func (registration *grant) finishCall(durablyResolved bool) {
	registration.mutex.Lock()
	defer registration.mutex.Unlock()
	if !durablyResolved {
		registration.unresolved = true
	}
	registration.admitted--
	if registration.closed && registration.admitted == 0 {
		close(registration.drained)
	}
}

func (registration *grant) drainResult() error {
	registration.mutex.Lock()
	defer registration.mutex.Unlock()
	if registration.unresolved {
		return ErrMutationDrainUnresolved
	}
	return nil
}

func (gateway *Gateway) executeMutation(operationContext context.Context, lease store.AgentTurnLease, reservation store.MutationReservation, invocation Invocation) mutationOutcome {
	if err := gateway.finalizeMutation(func(ctx context.Context) error {
		_, err := gateway.store.StartMutation(ctx, lease, reservation.ID)
		return err
	}); err != nil {
		if failErr := gateway.finalizeMutation(func(ctx context.Context) error {
			return gateway.store.FailMutation(ctx, lease, reservation.ID, errors.New("mutation could not start"))
		}); failErr != nil {
			return mutationOutcome{message: "mutation durable state is unresolved"}
		}
		return mutationOutcome{message: "mutation could not start", durablyResolved: true}
	}
	if operationContext.Err() != nil {
		if markErr := gateway.finalizeMutation(func(ctx context.Context) error {
			return gateway.store.MarkMutationUnknown(ctx, lease, reservation.ID, errors.New("backend outcome is ambiguous"))
		}); markErr != nil {
			return mutationOutcome{message: "mutation durable state is unresolved"}
		}
		return mutationOutcome{message: "mutation outcome is unresolved", durablyResolved: true}
	}

	backendContext, cancelBackend := context.WithCancel(operationContext)
	watcherContext, cancelWatcher := context.WithCancel(backendContext)
	watcherDone := make(chan struct{})
	var watcherMutex sync.Mutex
	stoppingWatcher := false
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(gateway.mutationFenceCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watcherContext.Done():
				return
			case <-ticker.C:
				if err := gateway.store.ValidateTurnFence(watcherContext, lease); err != nil {
					watcherMutex.Lock()
					if !stoppingWatcher {
						cancelBackend()
					}
					watcherMutex.Unlock()
					return
				}
			}
		}
	}()

	result, err := gateway.backend.Execute(backendContext, invocation)
	watcherMutex.Lock()
	stoppingWatcher = true
	cancelWatcher()
	watcherMutex.Unlock()
	<-watcherDone
	operationCanceled := backendContext.Err() != nil
	cancelBackend()

	if operationCanceled || err != nil && (IsOutcomeUnknown(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		if markErr := gateway.finalizeMutation(func(ctx context.Context) error {
			return gateway.store.MarkMutationUnknown(ctx, lease, reservation.ID, errors.New("backend outcome is ambiguous"))
		}); markErr != nil {
			return mutationOutcome{message: "mutation durable state is unresolved"}
		}
		return mutationOutcome{message: "mutation outcome is unresolved", durablyResolved: true}
	}
	if err != nil || len(result) == 0 || !json.Valid(result) {
		if failErr := gateway.finalizeMutation(func(ctx context.Context) error {
			return gateway.store.FailMutation(ctx, lease, reservation.ID, errors.New("backend mutation failed"))
		}); failErr != nil {
			return mutationOutcome{message: "mutation durable state is unresolved"}
		}
		return mutationOutcome{message: "mutation failed", durablyResolved: true}
	}
	result, err = canonicalJSON(result)
	if err != nil {
		if failErr := gateway.finalizeMutation(func(ctx context.Context) error {
			return gateway.store.FailMutation(ctx, lease, reservation.ID, errors.New("backend returned an invalid result"))
		}); failErr != nil {
			return mutationOutcome{message: "mutation durable state is unresolved"}
		}
		return mutationOutcome{message: "mutation failed", durablyResolved: true}
	}
	if err := gateway.finalizeMutation(func(ctx context.Context) error {
		return gateway.store.CompleteMutation(ctx, lease, reservation.ID, result)
	}); err != nil {
		if markErr := gateway.finalizeMutation(func(ctx context.Context) error {
			return gateway.store.MarkMutationUnknown(ctx, lease, reservation.ID, errors.New("mutation completion is uncertain"))
		}); markErr != nil {
			return mutationOutcome{message: "mutation durable state is unresolved"}
		}
		return mutationOutcome{message: "mutation outcome is unresolved", durablyResolved: true}
	}
	return mutationOutcome{result: result, durablyResolved: true}
}

func (gateway *Gateway) finalizeMutation(operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(gateway.lifecycle), gateway.mutationFinalizationTimeout)
	defer cancel()
	return operation(ctx)
}

func requiredStringArgument(arguments json.RawMessage, name string) (string, error) {
	var values map[string]json.RawMessage
	if json.Unmarshal(arguments, &values) != nil {
		return "", errors.New("invalid arguments")
	}
	var value string
	if json.Unmarshal(values[name], &value) != nil || value == "" || name == "operation_id" && !validOperationID(value) {
		return "", errors.New("missing argument")
	}
	return value, nil
}

func validOperationID(value string) bool {
	if len(value) == 0 || len(value) > 128 || !asciiAlphaNumeric(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !asciiAlphaNumeric(value[index]) && !strings.ContainsRune("-_.:", rune(value[index])) {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func mutationMetadata(tool string, scope TokenScope) MutationMetadata {
	metadata := MutationMetadata{ExternalService: "github"}
	switch tool {
	case ToolPublishChanges:
		metadata.ExternalService = "git"
		metadata.ExternalResourceID = fmt.Sprintf("%d:%s", scope.Repository.ID, scope.Branch)
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolOpenPR:
		metadata.ExternalResourceID = fmt.Sprintf("%d:%s:%s", scope.Repository.ID, scope.Branch, scope.DefaultBranch)
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolRequestReview:
		metadata.ExternalService = "omnigrex"
		metadata.ExternalResourceID = fmt.Sprintf("%d:%s", scope.Repository.ID, scope.Branch)
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolReportBlocked:
		metadata.ExternalService = "omnigrex"
		metadata.ExternalResourceID = scope.WorkflowID
	case ToolSubmitReview, ToolCommentOnPullRequest:
		if scope.PullRequest != nil {
			metadata.ExternalResourceID = fmt.Sprintf("%d:%d", scope.Repository.ID, scope.PullRequest.ID)
		}
		metadata.ExpectedSHA = scope.HeadSHA
	case ToolCommentOnIssue:
		metadata.ExternalResourceID = fmt.Sprintf("%d:%d", scope.Repository.ID, scope.Issue.ID)
	}
	return metadata
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid JSON")
	}
	return json.Marshal(value)
}

func backendScope(scope TokenScope) ToolScope {
	var pullRequest *PullRequestScope
	if scope.PullRequest != nil {
		clone := *scope.PullRequest
		pullRequest = &clone
	}
	return ToolScope{
		WorkflowID: scope.WorkflowID, AgentAssignmentID: scope.Lease.AgentAssignmentID,
		AgentSessionID: scope.Lease.AgentSessionID, AgentTurnID: scope.Lease.ID,
		ExecutionEpoch: scope.Lease.ExecutionEpoch, Role: scope.Role,
		Repository: scope.Repository, Issue: scope.Issue, PullRequest: pullRequest,
		Branch: scope.Branch, DefaultBranch: scope.DefaultBranch, HeadSHA: scope.HeadSHA,
		TurnCreatedAt: scope.Lease.CreatedAt,
	}
}

func slicesContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func writeToolResult(response http.ResponseWriter, id json.RawMessage, result json.RawMessage) {
	writeRPCResult(response, id, map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(result)}},
	})
}

func writeToolError(response http.ResponseWriter, id json.RawMessage, message string) {
	writeRPCResult(response, id, map[string]any{
		"content": []map[string]string{{"type": "text", "text": message}},
		"isError": true,
	})
}

func (gateway *Gateway) completeInitialization(response http.ResponseWriter, request *http.Request, registration *grant, rpc rpcRequest) {
	if len(rpc.ID) != 0 || request.Header.Get("MCP-Protocol-Version") != ProtocolVersion || !emptyParams(rpc.Params) {
		http.Error(response, "MCP initialization is invalid", http.StatusBadRequest)
		return
	}
	registration.mutex.Lock()
	valid := registration.initializing
	if valid {
		registration.initializing = false
		registration.initialized = true
	}
	registration.mutex.Unlock()
	if !valid {
		http.Error(response, "MCP initialization is invalid", http.StatusBadRequest)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func (gateway *Gateway) listTools(response http.ResponseWriter, request *http.Request, registration *grant, rpc rpcRequest) {
	if !ready(request, registration) || len(rpc.ID) == 0 {
		http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
		return
	}
	var params struct {
		Cursor string `json:"cursor"`
	}
	if len(rpc.Params) != 0 {
		decoder := json.NewDecoder(strings.NewReader(string(rpc.Params)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&params) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeRPCError(response, rpc.ID, -32602, "invalid tools/list params")
			return
		}
	}
	tools, _ := toolsForScope(registration.scope)
	writeRPCResult(response, rpc.ID, map[string]any{"tools": tools})
}

func ready(request *http.Request, registration *grant) bool {
	if request.Header.Get("MCP-Protocol-Version") != ProtocolVersion {
		return false
	}
	registration.mutex.Lock()
	defer registration.mutex.Unlock()
	return registration.initialized
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (gateway *Gateway) initialize(response http.ResponseWriter, request *http.Request, registration *grant, rpc rpcRequest) {
	if len(rpc.ID) == 0 || (request.Header.Get("MCP-Protocol-Version") != "" && request.Header.Get("MCP-Protocol-Version") != ProtocolVersion) {
		writeRPCError(response, rpc.ID, -32600, "invalid initialize request")
		return
	}
	var params struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(rpc.Params)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&params) != nil || decoder.Decode(&struct{}{}) != io.EOF || params.ProtocolVersion != ProtocolVersion || params.Capabilities == nil || params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
		writeRPCError(response, rpc.ID, -32602, "invalid initialize params")
		return
	}
	registration.mutex.Lock()
	registration.initializing = true
	registration.initialized = false
	registration.mutex.Unlock()
	writeRPCResult(response, rpc.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
		"serverInfo":      map[string]string{"name": "omnigrex", "version": "1.0.0"},
	})
}

func validRequestID(raw json.RawMessage) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	switch id := value.(type) {
	case string:
		return true
	case json.Number:
		_, err := id.Int64()
		return err == nil
	default:
		return false
	}
}

func emptyParams(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var params map[string]json.RawMessage
	return json.Unmarshal(raw, &params) == nil && len(params) == 0
}

func (gateway *Gateway) authenticate(request *http.Request) (*grant, bool) {
	if len(request.Header.Values("Authorization")) != 1 {
		return nil, false
	}
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || strings.Contains(header[len("Bearer "):], " ") || len(header) <= len("Bearer ") {
		return nil, false
	}
	hash := sha256.Sum256([]byte(header[len("Bearer "):]))
	gateway.mutex.RLock()
	registration := gateway.registrations[hash]
	gateway.mutex.RUnlock()
	if registration == nil || subtle.ConstantTimeCompare(hash[:], registration.tokenHash[:]) != 1 || !gateway.grantLive(registration) {
		return nil, false
	}
	return registration, true
}

func (gateway *Gateway) grantLive(registration *grant) bool {
	if registration == nil {
		return false
	}
	gateway.mutex.RLock()
	current := gateway.registrations[registration.tokenHash]
	gateway.mutex.RUnlock()
	return current == registration
}

func parsedPath(endpointURL string) string {
	parsed, _ := url.Parse(endpointURL)
	if parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

func accepts(header, wanted string) bool {
	for value := range strings.SplitSeq(header, ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || !strings.EqualFold(mediaType, wanted) {
			continue
		}
		quality := 1.0
		if text, present := parameters["q"]; present {
			quality, err = strconv.ParseFloat(text, 64)
			if err != nil {
				continue
			}
		}
		if quality > 0 && quality <= 1 {
			return true
		}
	}
	return false
}

func hasMediaType(header, wanted string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(mediaType, wanted)
}

func writeRPCResult(response http.ResponseWriter, id json.RawMessage, result any) {
	writeRPCJSON(response, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(response http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeRPCJSON(response, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func writeRPCJSON(response http.ResponseWriter, payload any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(payload)
}
