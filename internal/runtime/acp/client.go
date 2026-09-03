package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

var (
	ErrCapabilityUnsupported    = errors.New("ACP capability is unsupported")
	ErrPromptActive             = errors.New("ACP session already has an active prompt")
	ErrProtocolVersion          = errors.New("unsupported ACP protocol version")
	ErrSessionAmbiguous         = errors.New("multiple ACP sessions match the assignment")
	ErrSessionDiscoveryMismatch = errors.New("ACP session discovery result does not match the assignment")
	ErrSessionNotFound          = errors.New("no ACP session matches the assignment")
	ErrRequiredCapability       = errors.New("required ACP capability is unsupported")
	ErrUnknownStopReason        = errors.New("unknown ACP stop reason")
	ErrUnknownSession           = errors.New("unknown ACP session")
	ErrWorkspacePath            = errors.New("ACP session must use the stable workspace path")
	ErrSessionIDEmpty           = errors.New("ACP session id is empty")
	ErrSessionDiscoveryLimit    = errors.New("ACP session discovery limit exceeded")
)

const (
	maxSessionDiscoveryPages   = 100
	maxSessionDiscoveryResults = 1000
	maxSessionsPerPage         = 100
)

type ClientOptions struct {
	ClientInfo              Implementation
	RequiredCapabilities    RequiredCapabilities
	AgentEventSink          agentevent.Sink
	OnUpdate                func(context.Context, SessionUpdate)
	DecidePermission        func(context.Context, PermissionRequest) PermissionDecision
	CancellationGracePeriod time.Duration
}

type RequiredCapabilities struct {
	SessionList   bool
	SessionResume bool
	SessionLoad   bool
}

type Client struct {
	connection *Connection
	options    ClientOptions

	mutex               sync.Mutex
	initialized         bool
	capabilities        AgentCapabilities
	agentEventContext   agentevent.Context
	sessions            map[string]struct{}
	activePrompts       map[string]struct{}
	promptContexts      map[string]context.Context
	promptCancellations map[string]context.CancelFunc
}

func NewClient(transport io.ReadWriteCloser, options ClientOptions) *Client {
	if options.ClientInfo.Name == "" {
		options.ClientInfo = Implementation{Name: "omnigrex", Version: "dev"}
	}
	if options.CancellationGracePeriod <= 0 {
		options.CancellationGracePeriod = 5 * time.Second
	}
	if options.AgentEventSink == nil {
		options.AgentEventSink = agentevent.NoopSink{}
	}
	client := &Client{
		options:             options,
		sessions:            make(map[string]struct{}),
		activePrompts:       make(map[string]struct{}),
		promptContexts:      make(map[string]context.Context),
		promptCancellations: make(map[string]context.CancelFunc),
	}
	client.connection = NewConnection(transport, client)
	return client
}

func (client *Client) Initialize(ctx context.Context) (InitializeResponse, error) {
	request := map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs": map[string]bool{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false,
			"auth": map[string]bool{
				"terminal": false,
			},
		},
		"clientInfo": client.options.ClientInfo,
	}
	var response InitializeResponse
	if err := client.connection.Call(ctx, "initialize", request, &response); err != nil {
		return InitializeResponse{}, err
	}
	if response.ProtocolVersion != ProtocolVersion {
		_ = client.Close()
		return InitializeResponse{}, fmt.Errorf("%w: agent returned %d, want %d", ErrProtocolVersion, response.ProtocolVersion, ProtocolVersion)
	}
	if err := validateRequiredCapabilities(response.AgentCapabilities, client.options.RequiredCapabilities); err != nil {
		return response, err
	}

	client.mutex.Lock()
	client.initialized = true
	client.capabilities = response.AgentCapabilities
	client.mutex.Unlock()
	return response, nil
}

func (client *Client) CreateSession(ctx context.Context, request CreateSessionRequest) (Session, error) {
	if err := client.requireInitialized(); err != nil {
		return Session{}, err
	}
	if err := validateWorkspace(request.CWD); err != nil {
		return Session{}, err
	}
	params, err := client.sessionParams(request.CWD, request.MCPServers, request.AdditionalDirectories)
	if err != nil {
		return Session{}, err
	}

	var session Session
	if err := client.connection.Call(ctx, "session/new", params, &session); err != nil {
		return Session{}, err
	}
	if session.ID == "" {
		return Session{}, ErrSessionIDEmpty
	}
	client.recordSession(session.ID)
	return session, nil
}

func (client *Client) ContinueSession(ctx context.Context, request ContinueSessionRequest) error {
	_, err := client.ContinueSessionWithOptions(ctx, request)
	return err
}

func (client *Client) ContinueSessionWithOptions(ctx context.Context, request ContinueSessionRequest) ([]json.RawMessage, error) {
	if err := client.requireCapability("session/resume"); err != nil {
		return nil, err
	}
	if err := validateContinuation(request); err != nil {
		return nil, err
	}
	params, err := client.continuationParams(request)
	if err != nil {
		return nil, err
	}
	var response struct {
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if err := client.connection.Call(ctx, "session/resume", params, &response); err != nil {
		return nil, err
	}
	client.recordSession(request.SessionID)
	return response.ConfigOptions, nil
}

func (client *Client) ReplayHistory(ctx context.Context, request ContinueSessionRequest) error {
	_, err := client.ReplayHistoryWithOptions(ctx, request)
	return err
}

func (client *Client) ReplayHistoryWithOptions(ctx context.Context, request ContinueSessionRequest) ([]json.RawMessage, error) {
	if err := client.requireCapability("session/load"); err != nil {
		return nil, err
	}
	if err := validateContinuation(request); err != nil {
		return nil, err
	}
	params, err := client.continuationParams(request)
	if err != nil {
		return nil, err
	}
	var response struct {
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if err := client.connection.Call(ctx, "session/load", params, &response); err != nil {
		return nil, err
	}
	client.recordSession(request.SessionID)
	return response.ConfigOptions, nil
}

func (client *Client) DiscoverSessions(ctx context.Context, cwd, cursor string) (ListSessionsResponse, error) {
	if err := client.requireCapability("session/list"); err != nil {
		return ListSessionsResponse{}, err
	}
	if cwd != "" {
		if err := validateWorkspace(cwd); err != nil {
			return ListSessionsResponse{}, err
		}
	}
	params := make(map[string]string)
	if cwd != "" {
		params["cwd"] = cwd
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var response ListSessionsResponse
	if err := client.connection.Call(ctx, "session/list", params, &response); err != nil {
		return ListSessionsResponse{}, err
	}
	if len(response.Sessions) > maxSessionsPerPage {
		return ListSessionsResponse{}, ErrSessionDiscoveryLimit
	}
	for _, session := range response.Sessions {
		if session.ID == "" {
			return ListSessionsResponse{}, ErrSessionIDEmpty
		}
	}
	return response, nil
}

func (client *Client) RecoverCreatedSession(ctx context.Context, cwd string) (SessionInfo, error) {
	if err := validateWorkspace(cwd); err != nil {
		return SessionInfo{}, err
	}
	var recovered *SessionInfo
	cursor := ""
	seenCursors := make(map[string]struct{})
	resultCount := 0
	for pageNumber := 0; pageNumber < maxSessionDiscoveryPages; pageNumber++ {
		if _, seen := seenCursors[cursor]; seen {
			return SessionInfo{}, errors.New("session/list returned a repeated cursor")
		}
		seenCursors[cursor] = struct{}{}
		page, err := client.DiscoverSessions(ctx, cwd, cursor)
		if err != nil {
			return SessionInfo{}, err
		}
		resultCount += len(page.Sessions)
		if resultCount > maxSessionDiscoveryResults {
			return SessionInfo{}, ErrSessionDiscoveryLimit
		}
		for index := range page.Sessions {
			if filepath.Clean(page.Sessions[index].CWD) != filepath.Clean(cwd) {
				return SessionInfo{}, fmt.Errorf("%w: agent returned cwd %q", ErrSessionDiscoveryMismatch, page.Sessions[index].CWD)
			}
			if recovered != nil {
				return SessionInfo{}, ErrSessionAmbiguous
			}
			session := page.Sessions[index]
			recovered = &session
		}
		if page.NextCursor == "" {
			if recovered == nil {
				return SessionInfo{}, ErrSessionNotFound
			}
			return *recovered, nil
		}
		cursor = page.NextCursor
	}
	return SessionInfo{}, ErrSessionDiscoveryLimit
}

func (client *Client) SetConfigOption(ctx context.Context, sessionID, configID string, value any) ([]json.RawMessage, error) {
	if err := client.requireSession(sessionID); err != nil {
		return nil, err
	}
	params := map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	}
	if _, ok := value.(bool); ok {
		params["type"] = "boolean"
	}
	var response struct {
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if err := client.connection.Call(ctx, "session/set_config_option", params, &response); err != nil {
		return nil, err
	}
	return response.ConfigOptions, nil
}

func (client *Client) Prompt(ctx context.Context, sessionID string, prompt []ContentBlock) (PromptResponse, error) {
	if err := ctx.Err(); err != nil {
		return PromptResponse{}, err
	}
	if err := client.beginPrompt(sessionID); err != nil {
		return PromptResponse{}, err
	}
	permissionCtx, cancelPermissions := context.WithCancel(context.Background())
	client.mutex.Lock()
	client.promptContexts[sessionID] = permissionCtx
	client.promptCancellations[sessionID] = cancelPermissions
	client.mutex.Unlock()
	defer func() {
		cancelPermissions()
		client.endPrompt(sessionID)
	}()

	type promptResult struct {
		response PromptResponse
		err      error
	}
	completed := make(chan promptResult, 1)
	submitted := make(chan error, 1)
	go func() {
		var response PromptResponse
		err := client.connection.callWithContexts(ctx, context.Background(), "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    prompt,
		}, &response, submitted)
		completed <- promptResult{response: response, err: err}
	}()
	if err := <-submitted; err != nil {
		return PromptResponse{}, err
	}

	select {
	case result := <-completed:
		return validatePromptResponse(result.response, result.err)
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), client.options.CancellationGracePeriod)
		defer cancel()
		if err := client.CancelTurn(cancelCtx, sessionID); err != nil {
			_ = client.Close()
			select {
			case <-completed:
			case <-cancelCtx.Done():
			}
			return PromptResponse{}, fmt.Errorf("cancel ACP prompt: %w", err)
		}
		select {
		case result := <-completed:
			return validatePromptResponse(result.response, result.err)
		case <-cancelCtx.Done():
			_ = client.Close()
			return PromptResponse{}, fmt.Errorf("ACP prompt did not stop after cancellation: %w", cancelCtx.Err())
		}
	}
}

func (client *Client) CancelTurn(ctx context.Context, sessionID string) error {
	if err := client.requireSession(sessionID); err != nil {
		return err
	}
	client.cancelPermissionRequest(sessionID)
	return client.connection.Notify(ctx, "session/cancel", map[string]string{"sessionId": sessionID})
}

func (client *Client) Close() error {
	return client.connection.Close()
}

// SetAgentEventContext supplies durable execution context for subsequent ACP updates.
func (client *Client) SetAgentEventContext(eventContext agentevent.Context) {
	client.mutex.Lock()
	client.agentEventContext = eventContext
	client.mutex.Unlock()
}

func (client *Client) HandleNotification(ctx context.Context, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var update SessionUpdate
	if err := json.Unmarshal(params, &update); err != nil || update.SessionID == "" || len(update.Update) == 0 {
		return
	}
	client.mutex.Lock()
	eventContext := client.agentEventContext
	client.mutex.Unlock()
	if eventContext.AssignmentID != "" && eventContext.ACPSessionID == update.SessionID {
		_ = EmitAgentEvent(ctx, client.options.AgentEventSink, eventContext, time.Now(), update)
	}
	if client.options.OnUpdate != nil {
		client.options.OnUpdate(ctx, update)
	}
}

func (client *Client) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	if method != "session/request_permission" {
		return nil, NewRPCError(MethodNotFound, "method not found")
	}
	var request PermissionRequest
	if err := json.Unmarshal(params, &request); err != nil || request.SessionID == "" {
		return nil, NewRPCError(InvalidParams, "invalid permission request")
	}
	var toolCall struct {
		ID string `json:"toolCallId"`
	}
	if err := json.Unmarshal(request.ToolCall, &toolCall); err != nil || toolCall.ID == "" || !validPermissionOptions(request.Options) {
		return nil, NewRPCError(InvalidParams, "invalid permission request")
	}
	if err := client.requireSession(request.SessionID); err != nil {
		return cancelledPermissionResponse(), nil
	}

	permissionCtx, cancel, active := client.permissionContext(ctx, request.SessionID)
	defer cancel()
	if !active {
		return cancelledPermissionResponse(), nil
	}
	decision := PermissionDecision{}
	if client.options.DecidePermission == nil {
		decision = defaultPermissionDecision(request.Options)
	} else {
		decision = client.options.DecidePermission(permissionCtx, request)
	}
	if permissionCtx.Err() != nil || !offeredPermission(request.Options, decision.OptionID) {
		return cancelledPermissionResponse(), nil
	}
	return map[string]any{
		"outcome": map[string]string{
			"outcome":  "selected",
			"optionId": decision.OptionID,
		},
	}, nil
}

func (client *Client) requireInitialized() error {
	client.mutex.Lock()
	defer client.mutex.Unlock()
	if !client.initialized {
		return errors.New("ACP client is not initialized")
	}
	return nil
}

func (client *Client) requireCapability(method string) error {
	client.mutex.Lock()
	defer client.mutex.Unlock()
	if !client.initialized {
		return errors.New("ACP client is not initialized")
	}
	supported := false
	switch method {
	case "session/list":
		supported = client.capabilities.SupportsSessionList()
	case "session/resume":
		supported = client.capabilities.SupportsSessionResume()
	case "session/load":
		supported = client.capabilities.SupportsSessionLoad()
	}
	if !supported {
		return fmt.Errorf("%w: %s", ErrCapabilityUnsupported, method)
	}
	return nil
}

func (client *Client) recordSession(sessionID string) {
	if sessionID == "" {
		return
	}
	client.mutex.Lock()
	client.sessions[sessionID] = struct{}{}
	client.mutex.Unlock()
}

func (client *Client) requireSession(sessionID string) error {
	if sessionID == "" {
		return ErrSessionIDEmpty
	}
	client.mutex.Lock()
	defer client.mutex.Unlock()
	if _, ok := client.sessions[sessionID]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	return nil
}

func (client *Client) beginPrompt(sessionID string) error {
	if sessionID == "" {
		return ErrSessionIDEmpty
	}
	client.mutex.Lock()
	defer client.mutex.Unlock()
	if _, ok := client.sessions[sessionID]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	if _, ok := client.activePrompts[sessionID]; ok {
		return fmt.Errorf("%w: %s", ErrPromptActive, sessionID)
	}
	client.activePrompts[sessionID] = struct{}{}
	return nil
}

func (client *Client) endPrompt(sessionID string) {
	client.mutex.Lock()
	delete(client.activePrompts, sessionID)
	delete(client.promptContexts, sessionID)
	delete(client.promptCancellations, sessionID)
	client.mutex.Unlock()
}

func (client *Client) cancelPermissionRequest(sessionID string) {
	client.mutex.Lock()
	cancel := client.promptCancellations[sessionID]
	client.mutex.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (client *Client) permissionContext(
	fallback context.Context,
	sessionID string,
) (context.Context, context.CancelFunc, bool) {
	client.mutex.Lock()
	promptCtx := client.promptContexts[sessionID]
	_, active := client.activePrompts[sessionID]
	client.mutex.Unlock()
	if promptCtx == nil || !active {
		ctx, cancel := context.WithCancel(fallback)
		return ctx, cancel, false
	}
	ctx, cancel := context.WithCancel(promptCtx)
	stopFallback := context.AfterFunc(fallback, cancel)
	return ctx, func() {
		stopFallback()
		cancel()
	}, true
}

func (client *Client) sessionParams(
	cwd string,
	servers []MCPServer,
	additionalDirectories []string,
) (map[string]any, error) {
	client.mutex.Lock()
	capabilities := client.capabilities
	client.mutex.Unlock()
	if len(additionalDirectories) > 0 && !capabilities.SupportsAdditionalDirectories() {
		return nil, fmt.Errorf("%w: session additional directories", ErrCapabilityUnsupported)
	}
	for _, directory := range additionalDirectories {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			return nil, fmt.Errorf("invalid ACP additional directory %q", directory)
		}
	}
	validatedServers, err := validateMCPServers(servers, capabilities.MCPCapabilities)
	if err != nil {
		return nil, err
	}
	if servers == nil {
		validatedServers = []MCPServer{}
	}
	params := map[string]any{
		"cwd":        cwd,
		"mcpServers": validatedServers,
	}
	if len(additionalDirectories) > 0 {
		params["additionalDirectories"] = additionalDirectories
	}
	return params, nil
}

func (client *Client) continuationParams(request ContinueSessionRequest) (map[string]any, error) {
	params, err := client.sessionParams(request.CWD, request.MCPServers, request.AdditionalDirectories)
	if err != nil {
		return nil, err
	}
	params["sessionId"] = request.SessionID
	return params, nil
}

func validateContinuation(request ContinueSessionRequest) error {
	if request.SessionID == "" {
		return ErrSessionIDEmpty
	}
	return validateWorkspace(request.CWD)
}

func validateWorkspace(cwd string) error {
	if cwd != WorkspacePath {
		return fmt.Errorf("%w: got %q, want %q", ErrWorkspacePath, cwd, WorkspacePath)
	}
	return nil
}

func validateMCPServers(servers []MCPServer, capabilities MCPCapabilities) ([]MCPServer, error) {
	validated := make([]MCPServer, len(servers))
	for index, server := range servers {
		if server.Name == "" {
			return nil, errors.New("ACP MCP server name is empty")
		}
		switch server.Type {
		case "":
			if !filepath.IsAbs(server.Command) {
				return nil, fmt.Errorf("ACP MCP command must be absolute: %q", server.Command)
			}
			if server.Args == nil {
				server.Args = []string{}
			}
			if server.Env == nil {
				server.Env = []EnvironmentEntry{}
			}
			if !validEnvironmentEntries(server.Env) {
				return nil, fmt.Errorf("ACP MCP server %q has invalid environment", server.Name)
			}
		case "http":
			if !capabilities.HTTP {
				return nil, fmt.Errorf("%w: HTTP MCP", ErrCapabilityUnsupported)
			}
			if err := validateRemoteMCPServer(&server); err != nil {
				return nil, err
			}
		case "sse":
			if !capabilities.SSE {
				return nil, fmt.Errorf("%w: SSE MCP", ErrCapabilityUnsupported)
			}
			if err := validateRemoteMCPServer(&server); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported MCP server type %q", server.Type)
		}
		validated[index] = server
	}
	return validated, nil
}

func validateRemoteMCPServer(server *MCPServer) error {
	parsed, err := url.Parse(server.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("ACP MCP server %q has invalid URL", server.Name)
	}
	if server.Headers == nil {
		server.Headers = []EnvironmentEntry{}
	}
	if !validEnvironmentEntries(server.Headers) {
		return fmt.Errorf("ACP MCP server %q has invalid headers", server.Name)
	}
	return nil
}

func validEnvironmentEntries(entries []EnvironmentEntry) bool {
	for _, entry := range entries {
		if entry.Name == "" {
			return false
		}
	}
	return true
}

func validatePromptResponse(response PromptResponse, err error) (PromptResponse, error) {
	if err != nil {
		return PromptResponse{}, err
	}
	switch response.StopReason {
	case StopReasonEndTurn, StopReasonMaxTokens, StopReasonMaxTurnRequests, StopReasonRefusal, StopReasonCancelled:
		return response, nil
	default:
		return response, fmt.Errorf("%w %q", ErrUnknownStopReason, response.StopReason)
	}
}

func validateRequiredCapabilities(capabilities AgentCapabilities, required RequiredCapabilities) error {
	if required.SessionList && !capabilities.SupportsSessionList() {
		return fmt.Errorf("%w: session/list", ErrRequiredCapability)
	}
	if required.SessionResume && !capabilities.SupportsSessionResume() {
		return fmt.Errorf("%w: session/resume", ErrRequiredCapability)
	}
	if required.SessionLoad && !capabilities.SupportsSessionLoad() {
		return fmt.Errorf("%w: session/load", ErrRequiredCapability)
	}
	return nil
}

func defaultPermissionDecision(options []PermissionOption) PermissionDecision {
	for _, kind := range []string{"reject_once", "reject_always"} {
		for _, option := range options {
			if option.Kind == kind {
				return PermissionDecision{OptionID: option.ID}
			}
		}
	}
	return PermissionDecision{}
}

func offeredPermission(options []PermissionOption, optionID string) bool {
	if optionID == "" {
		return false
	}
	for _, option := range options {
		if option.ID == optionID {
			return true
		}
	}
	return false
}

func validPermissionOptions(options []PermissionOption) bool {
	if len(options) == 0 {
		return false
	}
	for _, option := range options {
		if option.ID == "" || option.Name == "" {
			return false
		}
		switch option.Kind {
		case "allow_once", "allow_always", "reject_once", "reject_always":
		default:
			return false
		}
	}
	return true
}

func cancelledPermissionResponse() map[string]any {
	return map[string]any{
		"outcome": map[string]string{"outcome": "cancelled"},
	}
}
