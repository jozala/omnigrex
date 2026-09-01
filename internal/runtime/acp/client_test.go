package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
)

func TestClientInitializesAndRecordsCapabilities(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initialized := make(chan struct {
		response acp.InitializeResponse
		err      error
	}, 1)
	go func() {
		response, err := client.Initialize(context.Background())
		initialized <- struct {
			response acp.InitializeResponse
			err      error
		}{response: response, err: err}
	}()

	request := readWireMessage(t, reader)
	if request.Method != "initialize" {
		t.Fatalf("method = %q, want %q", request.Method, "initialize")
	}
	var params struct {
		ProtocolVersion    int `json:"protocolVersion"`
		ClientCapabilities struct {
			FS struct {
				ReadTextFile  bool `json:"readTextFile"`
				WriteTextFile bool `json:"writeTextFile"`
			} `json:"fs"`
			Terminal bool `json:"terminal"`
		} `json:"clientCapabilities"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if params.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", params.ProtocolVersion)
	}
	if params.ClientCapabilities.FS.ReadTextFile || params.ClientCapabilities.FS.WriteTextFile || params.ClientCapabilities.Terminal {
		t.Errorf("unexpected client capabilities: %+v", params.ClientCapabilities)
	}

	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession": true,
				"sessionCapabilities": map[string]any{
					"list":   map[string]any{},
					"resume": map[string]any{},
				},
			},
			"agentInfo": map[string]string{"name": "Fake Agent", "version": "1.0.0"},
		},
	})

	got := <-initialized
	if got.err != nil {
		t.Fatalf("Initialize() error = %v", got.err)
	}
	if !got.response.AgentCapabilities.SupportsSessionList() {
		t.Error("session/list capability was not recorded")
	}
	if !got.response.AgentCapabilities.SupportsSessionResume() {
		t.Error("session/resume capability was not recorded")
	}
	if !got.response.AgentCapabilities.SupportsSessionLoad() {
		t.Error("session/load capability was not recorded")
	}
}

func TestClientRejectsUnsupportedProtocolVersion(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initialized := make(chan error, 1)
	go func() {
		_, err := client.Initialize(context.Background())
		initialized <- err
	}()

	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]any{"protocolVersion": 2},
	})
	if err := <-initialized; !errors.Is(err, acp.ErrProtocolVersion) {
		t.Fatalf("Initialize() error = %v, want ErrProtocolVersion", err)
	}
}

func TestClientRejectsMissingRequiredCapability(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{
		RequiredCapabilities: acp.RequiredCapabilities{SessionResume: true},
	})
	reader := bufio.NewReader(agent)
	initialized := make(chan error, 1)
	go func() {
		_, err := client.Initialize(context.Background())
		initialized <- err
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}},
	})
	if err := <-initialized; !errors.Is(err, acp.ErrRequiredCapability) {
		t.Fatalf("Initialize() error = %v, want ErrRequiredCapability", err)
	}
}

func TestClientCreatesContinuesReplaysAndListsSession(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"loadSession": true,
		"sessionCapabilities": map[string]any{
			"list":   map[string]any{},
			"resume": map[string]any{},
		},
	})

	if _, err := client.CreateSession(context.Background(), acp.CreateSessionRequest{CWD: "/tmp/workspace"}); !errors.Is(err, acp.ErrWorkspacePath) {
		t.Fatalf("CreateSession() error = %v, want ErrWorkspacePath", err)
	}

	created := make(chan struct {
		session acp.Session
		err     error
	}, 1)
	go func() {
		session, err := client.CreateSession(context.Background(), acp.CreateSessionRequest{CWD: acp.WorkspacePath})
		created <- struct {
			session acp.Session
			err     error
		}{session: session, err: err}
	}()
	newRequest := readWireMessage(t, reader)
	if newRequest.Method != "session/new" {
		t.Fatalf("method = %q, want %q", newRequest.Method, "session/new")
	}
	assertSessionParams(t, newRequest.Params, "", acp.WorkspacePath)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      newRequest.ID,
		"result":  map[string]string{"sessionId": "session-1"},
	})
	newResult := <-created
	if newResult.err != nil {
		t.Fatalf("CreateSession() error = %v", newResult.err)
	}
	if newResult.session.ID != "session-1" {
		t.Fatalf("session ID = %q, want %q", newResult.session.ID, "session-1")
	}

	continued := make(chan error, 1)
	go func() {
		continued <- client.ContinueSession(context.Background(), acp.ContinueSessionRequest{
			SessionID: "session-1",
			CWD:       acp.WorkspacePath,
		})
	}()
	resumeRequest := readWireMessage(t, reader)
	if resumeRequest.Method != "session/resume" {
		t.Fatalf("method = %q, want %q", resumeRequest.Method, "session/resume")
	}
	assertSessionParams(t, resumeRequest.Params, "session-1", acp.WorkspacePath)
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": resumeRequest.ID, "result": map[string]any{}})
	if err := <-continued; err != nil {
		t.Fatalf("ContinueSession() error = %v", err)
	}

	replayed := make(chan error, 1)
	go func() {
		replayed <- client.ReplayHistory(context.Background(), acp.ContinueSessionRequest{
			SessionID: "session-1",
			CWD:       acp.WorkspacePath,
		})
	}()
	loadRequest := readWireMessage(t, reader)
	if loadRequest.Method != "session/load" {
		t.Fatalf("method = %q, want %q", loadRequest.Method, "session/load")
	}
	assertSessionParams(t, loadRequest.Params, "session-1", acp.WorkspacePath)
	writeWireMessage(t, agent, map[string]any{"jsonrpc": "2.0", "id": loadRequest.ID, "result": map[string]any{}})
	if err := <-replayed; err != nil {
		t.Fatalf("ReplayHistory() error = %v", err)
	}

	listed := make(chan struct {
		result acp.ListSessionsResponse
		err    error
	}, 1)
	go func() {
		result, err := client.DiscoverSessions(context.Background(), acp.WorkspacePath, "")
		listed <- struct {
			result acp.ListSessionsResponse
			err    error
		}{result: result, err: err}
	}()
	listRequest := readWireMessage(t, reader)
	if listRequest.Method != "session/list" {
		t.Fatalf("method = %q, want %q", listRequest.Method, "session/list")
	}
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      listRequest.ID,
		"result": map[string]any{
			"sessions": []map[string]string{{"sessionId": "session-1", "cwd": acp.WorkspacePath}},
		},
	})
	listResult := <-listed
	if listResult.err != nil {
		t.Fatalf("DiscoverSessions() error = %v", listResult.err)
	}
	if len(listResult.result.Sessions) != 1 || listResult.result.Sessions[0].ID != "session-1" {
		t.Fatalf("sessions = %+v, want session-1", listResult.result.Sessions)
	}
}

func TestClientRejectsUnstableWorkspaceForContinuationAndReplay(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"loadSession": true,
		"sessionCapabilities": map[string]any{
			"resume": map[string]any{},
		},
	})
	request := acp.ContinueSessionRequest{SessionID: "session-1", CWD: "/tmp/workspace"}
	if err := client.ContinueSession(context.Background(), request); !errors.Is(err, acp.ErrWorkspacePath) {
		t.Errorf("ContinueSession() error = %v, want ErrWorkspacePath", err)
	}
	if err := client.ReplayHistory(context.Background(), request); !errors.Is(err, acp.ErrWorkspacePath) {
		t.Errorf("ReplayHistory() error = %v, want ErrWorkspacePath", err)
	}
}

func TestClientSetsBooleanConfigOption(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	configured := make(chan error, 1)
	go func() {
		_, err := client.SetConfigOption(context.Background(), "session-1", "thinking", true)
		configured <- err
	}()
	request := readWireMessage(t, reader)
	var params struct {
		Type  string `json:"type"`
		Value bool   `json:"value"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("decode config option params: %v", err)
	}
	if params.Type != "boolean" || !params.Value {
		t.Errorf("config option params = %+v", params)
	}
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]any{"configOptions": []any{}},
	})
	if err := <-configured; err != nil {
		t.Fatalf("SetConfigOption() error = %v", err)
	}
}

func TestClientPromptForwardsUpdatesRejectsConcurrencyAndCancels(t *testing.T) {
	updates := make(chan acp.SessionUpdate, 1)
	client, agent := newPipeClient(t, acp.ClientOptions{
		OnUpdate: func(_ context.Context, update acp.SessionUpdate) {
			updates <- update
		},
	})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	ctx, cancel := context.WithCancel(context.Background())
	prompted := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, err := client.Prompt(ctx, "session-1", []acp.ContentBlock{acp.TextContent("hello")})
		prompted <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: err}
	}()

	promptRequest := readWireMessage(t, reader)
	if promptRequest.Method != "session/prompt" {
		t.Fatalf("method = %q, want %q", promptRequest.Method, "session/prompt")
	}
	if _, err := client.Prompt(context.Background(), "session-1", []acp.ContentBlock{acp.TextContent("again")}); !errors.Is(err, acp.ErrPromptActive) {
		t.Fatalf("second Prompt() error = %v, want ErrPromptActive", err)
	}

	update := json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"working"}}`)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": "session-1",
			"update":    update,
		},
	})
	cancel()
	cancelRequest := readWireMessage(t, reader)
	if cancelRequest.Method != "session/cancel" {
		t.Fatalf("cancel method = %q, want %q", cancelRequest.Method, "session/cancel")
	}
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      promptRequest.ID,
		"result":  map[string]string{"stopReason": "cancelled"},
	})

	result := <-prompted
	if result.err != nil {
		t.Fatalf("Prompt() error = %v", result.err)
	}
	if result.response.StopReason != acp.StopReasonCancelled {
		t.Errorf("stop reason = %q, want %q", result.response.StopReason, acp.StopReasonCancelled)
	}
	select {
	case got := <-updates:
		if got.SessionID != "session-1" || string(got.Update) != string(update) {
			t.Errorf("update = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("session update was not forwarded")
	}
}

func TestClientAppliesNotificationBackpressureWithoutClosingConnection(t *testing.T) {
	releaseUpdates := make(chan struct{})
	updateStarted := make(chan struct{})
	var startOnce sync.Once
	client, agent := newPipeClient(t, acp.ClientOptions{
		OnUpdate: func(_ context.Context, _ acp.SessionUpdate) {
			startOnce.Do(func() { close(updateStarted) })
			<-releaseUpdates
		},
	})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	prompted := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), "session-1", []acp.ContentBlock{acp.TextContent("hello")})
		prompted <- err
	}()
	promptRequest := readWireMessage(t, reader)
	notification, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]string{"type": "text", "text": "chunk"},
			},
		},
	})
	if err != nil {
		t.Fatalf("encode notification: %v", err)
	}
	notification = append(notification, '\n')
	writesDone := make(chan error, 1)
	go func() {
		for range 300 {
			if _, err := agent.Write(notification); err != nil {
				writesDone <- err
				return
			}
		}
		writesDone <- nil
	}()
	select {
	case <-updateStarted:
	case <-time.After(time.Second):
		t.Fatal("notification consumer did not start")
	}
	select {
	case err := <-prompted:
		t.Fatalf("Prompt() ended while notifications were backpressured: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseUpdates)
	if err := <-writesDone; err != nil {
		t.Fatalf("write notifications: %v", err)
	}
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      promptRequest.ID,
		"result":  map[string]string{"stopReason": "end_turn"},
	})
	if err := <-prompted; err != nil {
		t.Fatalf("Prompt() error after notification consumer resumed = %v", err)
	}
}

func TestClientPreservesUnknownPromptStopReason(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	prompted := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, err := client.Prompt(context.Background(), "session-1", []acp.ContentBlock{acp.TextContent("hello")})
		prompted <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: err}
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]string{"stopReason": "end_of_generation"},
	})
	result := <-prompted
	if !errors.Is(result.err, acp.ErrUnknownStopReason) {
		t.Fatalf("Prompt() error = %v, want ErrUnknownStopReason", result.err)
	}
	if result.response.StopReason != "end_of_generation" {
		t.Fatalf("Prompt() stop reason = %q, want end_of_generation", result.response.StopReason)
	}
}

func TestClientDoesNotSubmitPromptWithCancelledContext(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Prompt(ctx, "session-1", []acp.ContentBlock{acp.TextContent("never sent")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt() error = %v, want context.Canceled", err)
	}
	if err := agent.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set fake agent deadline: %v", err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("cancelled prompt was sent to the agent")
	}
}

func TestClientCancellationInterruptsBlockedPromptSubmission(t *testing.T) {
	clientSide, agent := net.Pipe()
	transport := newBlockableTransport(clientSide)
	client := acp.NewClient(transport, acp.ClientOptions{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = agent.Close()
	})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	writeStarted := transport.BlockWrites()
	ctx, cancel := context.WithCancel(context.Background())
	prompted := make(chan error, 1)
	go func() {
		_, err := client.Prompt(ctx, "session-1", []acp.ContentBlock{acp.TextContent("blocked")})
		prompted <- err
	}()
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("prompt submission did not begin writing")
	}
	cancel()
	select {
	case err := <-prompted:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt() ignored cancellation during blocked submission")
	}
}

func TestClientValidatesNegotiatedSessionExtensions(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"mcpCapabilities": map[string]any{"http": true},
		"sessionCapabilities": map[string]any{
			"additionalDirectories": map[string]any{},
		},
	})

	created := make(chan error, 1)
	go func() {
		_, err := client.CreateSession(context.Background(), acp.CreateSessionRequest{
			CWD:                   acp.WorkspacePath,
			AdditionalDirectories: []string{"/context"},
			MCPServers: []acp.MCPServer{{
				Type: "http",
				Name: "tools",
				URL:  "https://tools.example.test/mcp",
			}},
		})
		created <- err
	}()
	request := readWireMessage(t, reader)
	var params struct {
		AdditionalDirectories []string          `json:"additionalDirectories"`
		MCPServers            []json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatalf("decode session params: %v", err)
	}
	if len(params.AdditionalDirectories) != 1 || params.AdditionalDirectories[0] != "/context" {
		t.Errorf("additional directories = %v", params.AdditionalDirectories)
	}
	if len(params.MCPServers) != 1 {
		t.Fatalf("MCP servers = %d, want 1", len(params.MCPServers))
	}
	var server map[string]json.RawMessage
	if err := json.Unmarshal(params.MCPServers[0], &server); err != nil {
		t.Fatalf("decode MCP server: %v", err)
	}
	if _, present := server["headers"]; !present {
		t.Error("HTTP MCP server omitted required headers array")
	}
	if _, present := server["args"]; present {
		t.Error("HTTP MCP server included stdio args")
	}
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]string{"sessionId": "session-1"},
	})
	if err := <-created; err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
}

func TestClientRejectsUnnegotiatedSessionExtensions(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})

	_, err := client.CreateSession(context.Background(), acp.CreateSessionRequest{
		CWD:                   acp.WorkspacePath,
		AdditionalDirectories: []string{"/context"},
	})
	if !errors.Is(err, acp.ErrCapabilityUnsupported) {
		t.Fatalf("CreateSession() additional directory error = %v, want ErrCapabilityUnsupported", err)
	}
	_, err = client.CreateSession(context.Background(), acp.CreateSessionRequest{
		CWD: acp.WorkspacePath,
		MCPServers: []acp.MCPServer{{
			Type: "http",
			Name: "tools",
			URL:  "https://tools.example.test/mcp",
		}},
	})
	if !errors.Is(err, acp.ErrCapabilityUnsupported) {
		t.Fatalf("CreateSession() HTTP MCP error = %v, want ErrCapabilityUnsupported", err)
	}
}

func TestClientDeniesPermissionWithoutPolicy(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{})
	createSession(t, client, agent, reader, "session-1")

	prompted := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), "session-1", []acp.ContentBlock{acp.TextContent("hello")})
		prompted <- err
	}()
	promptRequest := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": "session-1",
			"toolCall":  map[string]string{"toolCallId": "tool-1", "title": "write file"},
			"options": []map[string]string{
				{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"},
				{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
			},
		},
	})
	permissionResponse := readWireMessage(t, reader)
	var response struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(permissionResponse.Result, &response); err != nil {
		t.Fatalf("decode permission response: %v", err)
	}
	if response.Outcome.Outcome != "selected" || response.Outcome.OptionID != "reject-once" {
		t.Errorf("permission outcome = %+v, want reject-once", response.Outcome)
	}

	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      promptRequest.ID,
		"result":  map[string]string{"stopReason": "end_turn"},
	})
	if err := <-prompted; err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
}

func TestClientRecoversExactlyOneCreatedSession(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"sessionCapabilities": map[string]any{"list": map[string]any{}},
	})

	recovered := make(chan struct {
		session acp.SessionInfo
		err     error
	}, 1)
	go func() {
		session, err := client.RecoverCreatedSession(context.Background(), acp.WorkspacePath)
		recovered <- struct {
			session acp.SessionInfo
			err     error
		}{session: session, err: err}
	}()
	firstPage := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      firstPage.ID,
		"result": map[string]any{
			"sessions":   []map[string]string{},
			"nextCursor": "page-2",
		},
	})
	secondPage := readWireMessage(t, reader)
	var secondPageParams struct {
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(secondPage.Params, &secondPageParams); err != nil {
		t.Fatalf("decode second page params: %v", err)
	}
	if secondPageParams.Cursor != "page-2" {
		t.Errorf("cursor = %q, want page-2", secondPageParams.Cursor)
	}
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      secondPage.ID,
		"result": map[string]any{
			"sessions": []map[string]string{{"sessionId": "session-1", "cwd": acp.WorkspacePath}},
		},
	})
	result := <-recovered
	if result.err != nil {
		t.Fatalf("RecoverCreatedSession() error = %v", result.err)
	}
	if result.session.ID != "session-1" {
		t.Fatalf("recovered session = %+v, want session-1", result.session)
	}
}

func TestClientRejectsAmbiguousCreatedSessionRecovery(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"sessionCapabilities": map[string]any{"list": map[string]any{}},
	})

	recovered := make(chan error, 1)
	go func() {
		_, err := client.RecoverCreatedSession(context.Background(), acp.WorkspacePath)
		recovered <- err
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"sessions": []map[string]string{
				{"sessionId": "session-1", "cwd": acp.WorkspacePath},
				{"sessionId": "session-2", "cwd": acp.WorkspacePath},
			},
		},
	})
	if err := <-recovered; !errors.Is(err, acp.ErrSessionAmbiguous) {
		t.Fatalf("RecoverCreatedSession() error = %v, want ErrSessionAmbiguous", err)
	}
}

func TestClientReportsMissingCreatedSession(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"sessionCapabilities": map[string]any{"list": map[string]any{}},
	})

	recovered := make(chan error, 1)
	go func() {
		_, err := client.RecoverCreatedSession(context.Background(), acp.WorkspacePath)
		recovered <- err
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]any{"sessions": []any{}},
	})
	if err := <-recovered; !errors.Is(err, acp.ErrSessionNotFound) {
		t.Fatalf("RecoverCreatedSession() error = %v, want ErrSessionNotFound", err)
	}
}

func TestClientRejectsCreatedSessionFromForeignWorkingDirectory(t *testing.T) {
	client, agent := newPipeClient(t, acp.ClientOptions{})
	reader := bufio.NewReader(agent)
	initializeClient(t, client, agent, reader, map[string]any{
		"sessionCapabilities": map[string]any{"list": map[string]any{}},
	})

	recovered := make(chan error, 1)
	go func() {
		_, err := client.RecoverCreatedSession(context.Background(), acp.WorkspacePath)
		recovered <- err
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"sessions": []map[string]string{{"sessionId": "foreign", "cwd": "/somewhere/else"}},
		},
	})
	if err := <-recovered; !errors.Is(err, acp.ErrSessionNotFound) {
		t.Fatalf("RecoverCreatedSession() error = %v, want ErrSessionNotFound", err)
	}
}

func newPipeClient(t *testing.T, options acp.ClientOptions) (*acp.Client, net.Conn) {
	t.Helper()

	clientSide, agentSide := net.Pipe()
	client := acp.NewClient(clientSide, options)
	t.Cleanup(func() {
		_ = client.Close()
		_ = agentSide.Close()
	})
	return client, agentSide
}

type blockableTransport struct {
	net.Conn
	mutex     sync.Mutex
	blocked   bool
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newBlockableTransport(connection net.Conn) *blockableTransport {
	return &blockableTransport{Conn: connection, closed: make(chan struct{})}
}

func (transport *blockableTransport) BlockWrites() <-chan struct{} {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	transport.blocked = true
	transport.started = make(chan struct{})
	return transport.started
}

func (transport *blockableTransport) Write(data []byte) (int, error) {
	transport.mutex.Lock()
	blocked := transport.blocked
	started := transport.started
	transport.mutex.Unlock()
	if !blocked {
		return transport.Conn.Write(data)
	}
	transport.startOnce.Do(func() {
		close(started)
	})
	<-transport.closed
	return 0, net.ErrClosed
}

func (transport *blockableTransport) Close() error {
	transport.closeOnce.Do(func() {
		close(transport.closed)
	})
	return transport.Conn.Close()
}

func (transport *blockableTransport) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		transport.closeOnce.Do(func() {
			close(transport.closed)
		})
	}
	return nil
}

func initializeClient(
	t *testing.T,
	client *acp.Client,
	agent net.Conn,
	reader *bufio.Reader,
	capabilities map[string]any,
) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(context.Background())
		done <- err
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"protocolVersion":   1,
			"agentCapabilities": capabilities,
		},
	})
	if err := <-done; err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func createSession(t *testing.T, client *acp.Client, agent net.Conn, reader *bufio.Reader, sessionID string) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		_, err := client.CreateSession(context.Background(), acp.CreateSessionRequest{CWD: acp.WorkspacePath})
		done <- err
	}()
	request := readWireMessage(t, reader)
	writeWireMessage(t, agent, map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]string{"sessionId": sessionID},
	})
	if err := <-done; err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
}

func assertSessionParams(t *testing.T, data json.RawMessage, sessionID, cwd string) {
	t.Helper()

	var params struct {
		SessionID  string            `json:"sessionId"`
		CWD        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		t.Fatalf("decode session params: %v", err)
	}
	if params.SessionID != sessionID || params.CWD != cwd {
		t.Errorf("session params = %+v, want sessionId=%q cwd=%q", params, sessionID, cwd)
	}
	if params.MCPServers == nil {
		t.Error("mcpServers is missing or null, want []")
	}
}
