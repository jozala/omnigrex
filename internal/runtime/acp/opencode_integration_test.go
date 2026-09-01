//go:build integration

package acp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
)

const openCodeImageTag = "omnigrex/opencode:1.18.19"

const (
	seedPrompt         = "SEED_ALPHA"
	firstMarker        = "FIRST_MARKER"
	continuationPrompt = "VERIFY_CONTINUATION"
	continuedMarker    = "CONTINUED"
	blockingPrompt     = "BLOCK_UNTIL_CANCELLED"
	workingMarker      = "WORKING"
	mcpPrompt          = "RUN_MCP_TOOL"
	mcpResultMarker    = "MCP_RESULT_CONFIRMED"
	mcpSideEffect      = "MCP_SIDE_EFFECT"
	recoveryPrompt     = "VERIFY_AFTER_INTERRUPTION"
	recoveryMarker     = "INTERRUPTION_RECOVERED"
	mcpProtocolVersion = "2025-11-25"
	localToolPrompt    = "RUN_LOCAL_TOOL"
	localToolCommand   = "printf LOCAL_TOOL_STARTED; sleep 120"
	localToolMarker    = "LOCAL_TOOL_STARTED"
)

func TestOpenCodeSessionSurvivesFreshContainers(t *testing.T) {
	image := localOpenCodeImage(t)
	providerConfig := startFakeProvider(t)
	mcp := startTestMCPServer(t)
	mcpServers := []acp.MCPServer{{
		Type: "http",
		Name: "compat",
		URL:  mcp.URL,
	}}
	workspaceVolume := uniqueDockerName("workspace")
	runtimeStateVolume := uniqueDockerName("runtime-state")
	miseVolume := uniqueDockerName("mise")
	createDockerVolume(t, workspaceVolume)
	createDockerVolume(t, runtimeStateVolume)
	createDockerVolume(t, miseVolume)

	firstUpdates := make(chan acp.SessionUpdate, 16)
	first := startOpenCode(t, image, workspaceVolume, runtimeStateVolume, miseVolume, providerConfig, firstUpdates)
	initializeOpenCode(t, first.client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	session, err := first.client.CreateSession(ctx, acp.CreateSessionRequest{
		CWD:        acp.WorkspacePath,
		MCPServers: mcpServers,
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateSession() error = %v\nOpenCode stderr:\n%s", err, first.stderr.String())
	}
	prompt(t, first, session.ID, seedPrompt)
	waitForAgentText(t, firstUpdates, firstMarker)
	prompt(t, first, session.ID, mcpPrompt)
	waitForAgentText(t, firstUpdates, mcpResultMarker)
	mcp.assertSingleCall(t, "echo", map[string]any{"value": "phase-2"})
	first.stop(t)

	secondUpdates := make(chan acp.SessionUpdate, 16)
	second := startOpenCode(t, image, workspaceVolume, runtimeStateVolume, miseVolume, providerConfig, secondUpdates)
	initializeOpenCode(t, second.client)
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	recovered, err := second.client.RecoverCreatedSession(ctx, acp.WorkspacePath)
	cancel()
	if err != nil {
		t.Fatalf("RecoverCreatedSession() error = %v\nOpenCode stderr:\n%s", err, second.stderr.String())
	}
	if recovered.ID != session.ID {
		t.Fatalf("recovered session ID = %q, want %q", recovered.ID, session.ID)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	err = second.client.ReplayHistory(ctx, acp.ContinueSessionRequest{
		SessionID:  session.ID,
		CWD:        acp.WorkspacePath,
		MCPServers: mcpServers,
	})
	cancel()
	if err != nil {
		t.Fatalf("ReplayHistory() error = %v\nOpenCode stderr:\n%s", err, second.stderr.String())
	}
	waitForAgentText(t, secondUpdates, firstMarker)
	second.stop(t)

	thirdUpdates := make(chan acp.SessionUpdate, 16)
	third := startOpenCode(t, image, workspaceVolume, runtimeStateVolume, miseVolume, providerConfig, thirdUpdates)
	initializeOpenCode(t, third.client)
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	err = third.client.ContinueSession(ctx, acp.ContinueSessionRequest{
		SessionID:  session.ID,
		CWD:        acp.WorkspacePath,
		MCPServers: mcpServers,
	})
	cancel()
	if err != nil {
		t.Fatalf("ContinueSession() error = %v\nOpenCode stderr:\n%s", err, third.stderr.String())
	}
	prompt(t, third, session.ID, continuationPrompt)
	waitForAgentText(t, thirdUpdates, continuedMarker)
	third.stop(t)
	assertRuntimeStateExcludes(t, runtimeStateVolume, "not-a-real-secret")
}

func TestOpenCodePromptCancellationReturnsTerminalResult(t *testing.T) {
	image := localOpenCodeImage(t)
	providerConfig := startFakeProvider(t)
	workspaceVolume := uniqueDockerName("cancel-workspace")
	runtimeStateVolume := uniqueDockerName("cancel-runtime-state")
	miseVolume := uniqueDockerName("cancel-mise")
	createDockerVolume(t, workspaceVolume)
	createDockerVolume(t, runtimeStateVolume)
	createDockerVolume(t, miseVolume)

	updates := make(chan acp.SessionUpdate, 16)
	process := startOpenCode(t, image, workspaceVolume, runtimeStateVolume, miseVolume, providerConfig, updates)
	initializeOpenCode(t, process.client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	session, err := process.client.CreateSession(ctx, acp.CreateSessionRequest{CWD: acp.WorkspacePath})
	cancel()
	if err != nil {
		t.Fatalf("CreateSession() error = %v\nOpenCode stderr:\n%s", err, process.stderr.String())
	}

	promptCtx, cancelPrompt := context.WithCancel(context.Background())
	completed := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, err := process.client.Prompt(promptCtx, session.ID, []acp.ContentBlock{acp.TextContent(blockingPrompt)})
		completed <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: err}
	}()
	waitForAgentText(t, updates, workingMarker)
	cancelPrompt()

	select {
	case result := <-completed:
		if result.err != nil {
			t.Fatalf("cancelled Prompt() error = %v\nOpenCode stderr:\n%s", result.err, process.stderr.String())
		}
		if result.response.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancelled Prompt() stop reason = %q, want %q", result.response.StopReason, acp.StopReasonCancelled)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled Prompt() did not return a terminal result")
	}
	process.stop(t)
}

func TestOpenCodeSessionContinuesAfterProviderStreamInterruption(t *testing.T) {
	testOpenCodeSessionContinuesAfterInterruption(t, interruptionTestCase{
		purpose: "provider-stream",
		prompt:  blockingPrompt,
		wait: func(t *testing.T, updates <-chan acp.SessionUpdate) {
			waitForAgentText(t, updates, workingMarker)
		},
	})
}

func TestOpenCodeSessionContinuesAfterLocalToolInterruption(t *testing.T) {
	testOpenCodeSessionContinuesAfterInterruption(t, interruptionTestCase{
		purpose: "local-tool",
		prompt:  localToolPrompt,
		wait: func(t *testing.T, updates <-chan acp.SessionUpdate) {
			waitForToolOutput(t, updates, localToolMarker)
		},
	})
}

func TestOpenCodeSessionContinuesAfterMCPCallInterruption(t *testing.T) {
	testOpenCodeSessionContinuesAfterMCPInterruption(t, mcpBlockBeforeSideEffect)
}

func TestOpenCodeSessionContinuesAfterMCPResultInterruption(t *testing.T) {
	testOpenCodeSessionContinuesAfterMCPInterruption(t, mcpBlockAfterSideEffect)
}

func testOpenCodeSessionContinuesAfterMCPInterruption(t *testing.T, blockPoint mcpBlockPoint) {
	t.Helper()

	mcp := startMCPServer(t, blockPoint)
	mcpServers := []acp.MCPServer{{
		Type: "http",
		Name: "compat",
		URL:  mcp.URL,
	}}
	purpose := "mcp-call"
	if blockPoint == mcpBlockAfterSideEffect {
		purpose = "mcp-result"
	}
	testOpenCodeSessionContinuesAfterInterruption(t, interruptionTestCase{
		purpose:    purpose,
		prompt:     mcpPrompt,
		mcpServers: mcpServers,
		wait: func(t *testing.T, _ <-chan acp.SessionUpdate) {
			call := mcp.waitForCall(t)
			if call.Name != "echo" || !mapsEqual(call.Arguments, map[string]any{"value": "phase-2"}) {
				t.Fatalf("MCP tool call = %+v, want echo with phase-2", call)
			}
		},
		verify: func(t *testing.T) {
			if blockPoint == mcpBlockAfterSideEffect {
				mcp.assertSingleCall(t, "echo", map[string]any{"value": "phase-2"})
			} else {
				mcp.assertNoCalls(t)
			}
		},
	})
}

type interruptionTestCase struct {
	purpose    string
	prompt     string
	mcpServers []acp.MCPServer
	wait       func(*testing.T, <-chan acp.SessionUpdate)
	verify     func(*testing.T)
}

func testOpenCodeSessionContinuesAfterInterruption(t *testing.T, testCase interruptionTestCase) {
	t.Helper()

	image := localOpenCodeImage(t)
	providerConfig := startFakeProvider(t)
	workspaceVolume := uniqueDockerName(testCase.purpose + "-workspace")
	runtimeStateVolume := uniqueDockerName(testCase.purpose + "-runtime-state")
	miseVolume := uniqueDockerName(testCase.purpose + "-mise")
	createDockerVolume(t, workspaceVolume)
	createDockerVolume(t, runtimeStateVolume)
	createDockerVolume(t, miseVolume)

	firstUpdates := make(chan acp.SessionUpdate, 16)
	first := startOpenCode(t, image, workspaceVolume, runtimeStateVolume, miseVolume, providerConfig, firstUpdates)
	initializeOpenCode(t, first.client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	session, err := first.client.CreateSession(ctx, acp.CreateSessionRequest{
		CWD:        acp.WorkspacePath,
		MCPServers: testCase.mcpServers,
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateSession() error = %v\nOpenCode stderr:\n%s", err, first.stderr.String())
	}

	promptCompleted := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := first.client.Prompt(ctx, session.ID, []acp.ContentBlock{acp.TextContent(testCase.prompt)})
		promptCompleted <- err
	}()
	testCase.wait(t, firstUpdates)
	first.kill(t)
	select {
	case err := <-promptCompleted:
		if err == nil {
			t.Fatalf("interrupted %s prompt unexpectedly completed", testCase.purpose)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("interrupted %s prompt did not return", testCase.purpose)
	}

	secondUpdates := make(chan acp.SessionUpdate, 16)
	second := startOpenCode(t, image, workspaceVolume, runtimeStateVolume, miseVolume, providerConfig, secondUpdates)
	initializeOpenCode(t, second.client)
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	err = second.client.ContinueSession(ctx, acp.ContinueSessionRequest{
		SessionID:  session.ID,
		CWD:        acp.WorkspacePath,
		MCPServers: testCase.mcpServers,
	})
	cancel()
	if err != nil {
		t.Fatalf("ContinueSession() after %s interruption error = %v\nOpenCode stderr:\n%s", testCase.purpose, err, second.stderr.String())
	}
	prompt(t, second, session.ID, recoveryPrompt)
	waitForAgentText(t, secondUpdates, recoveryMarker)
	if testCase.verify != nil {
		testCase.verify(t)
	}
	second.stop(t)
}

type openCodeProcess struct {
	client  *acp.Client
	engine  *dockerruntime.Engine
	process *dockerruntime.Process
	stderr  bytes.Buffer
	once    sync.Once
}

func startOpenCode(
	t *testing.T,
	image string,
	workspaceVolume string,
	runtimeStateVolume string,
	miseVolume string,
	providerConfig string,
	updates chan<- acp.SessionUpdate,
) *openCodeProcess {
	t.Helper()

	engine, err := dockerruntime.NewEngine(dockerruntime.EngineOptions{
		AgentNetwork:     "bridge",
		AllowHostGateway: true,
		RuntimePolicy: dockerruntime.RuntimePolicy{
			User:       "10001:10001",
			WorkingDir: acp.WorkspacePath,
			VolumeBindings: map[string]string{
				acp.WorkspacePath:                      workspaceVolume,
				"/home/opencode/.local/share/opencode": runtimeStateVolume,
				"/home/opencode/.local/share/mise":     miseVolume,
			},
			RequiredVolumeTargets: []string{
				acp.WorkspacePath,
				"/home/opencode/.local/share/opencode",
				"/home/opencode/.local/share/mise",
			},
			RequiredWritableVolumeTargets: []string{
				acp.WorkspacePath,
				"/home/opencode/.local/share/opencode",
				"/home/opencode/.local/share/mise",
			},
			AllowedTmpfsTargets: []string{
				"/home/opencode/.cache",
				"/home/opencode/.config",
				"/home/opencode/.local/state",
				"/home/opencode/.opencode",
				"/tmp/opencode",
			},
			RequireVolumeSubpaths: true,
			RequiredEnvironment: map[string]string{
				"OPENCODE_AUTH_CONTENT": "{}",
			},
			MaxTmpfsBytes: 128 << 20,
		},
	})
	if err != nil {
		t.Fatalf("create Docker Engine client: %v", err)
	}
	process := &openCodeProcess{engine: engine}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	process.process, err = engine.Start(ctx, dockerruntime.Spec{
		Name:       uniqueDockerName("process"),
		Image:      image,
		User:       "10001:10001",
		WorkingDir: acp.WorkspacePath,
		Command:    []string{"acp"},
		Environment: []string{
			"OPENCODE_AUTH_CONTENT={}",
			"OPENCODE_CONFIG_CONTENT=" + providerConfig,
			"OPENCODE_DISABLE_MODELS_FETCH=1",
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"OPENCODE_PURE=1",
		},
		Labels: map[string]string{
			"io.omnigrex.assignment":      "integration-test",
			"io.omnigrex.runtime-profile": "opencode-acp/v1",
		},
		Volumes: []dockerruntime.VolumeMount{
			{Name: workspaceVolume, Subpath: "assignment", Target: acp.WorkspacePath},
			{Name: runtimeStateVolume, Subpath: "assignment", Target: "/home/opencode/.local/share/opencode"},
			{Name: miseVolume, Subpath: "assignment", Target: "/home/opencode/.local/share/mise"},
		},
		Tmpfs: []dockerruntime.TmpfsMount{
			{Target: "/home/opencode/.cache", SizeBytes: 64 << 20},
			{Target: "/home/opencode/.config", SizeBytes: 16 << 20},
			{Target: "/home/opencode/.local/state", SizeBytes: 16 << 20},
			{Target: "/home/opencode/.opencode", SizeBytes: 16 << 20},
			{Target: "/tmp/opencode", SizeBytes: 64 << 20, Executable: true},
		},
		Network:     "bridge",
		ExtraHosts:  []string{"host.docker.internal:host-gateway"},
		MemoryBytes: 512 << 20,
		PIDsLimit:   128,
	}, &process.stderr)
	if err != nil {
		_ = engine.Close()
		t.Fatalf("start OpenCode Runtime Process: %v\nOpenCode stderr:\n%s", err, process.stderr.String())
	}
	process.client = acp.NewClient(process.process.Transport(), acp.ClientOptions{
		RequiredCapabilities: acp.RequiredCapabilities{
			SessionList:   true,
			SessionResume: true,
			SessionLoad:   true,
		},
		OnUpdate: func(_ context.Context, update acp.SessionUpdate) {
			updates <- update
		},
		DecidePermission: compatibilityPermissionDecision,
	})
	t.Cleanup(func() {
		process.stop(t)
	})
	return process
}

func prompt(t *testing.T, process *openCodeProcess, sessionID, text string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := process.client.Prompt(ctx, sessionID, []acp.ContentBlock{acp.TextContent(text)})
	if err != nil {
		t.Fatalf("Prompt(%q) error = %v\nOpenCode stderr:\n%s", text, err, process.stderr.String())
	}
	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("Prompt(%q) stop reason = %q, want %q", text, response.StopReason, acp.StopReasonEndTurn)
	}
}

func waitForAgentText(t *testing.T, updates <-chan acp.SessionUpdate, want string) {
	t.Helper()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-updates:
			var event struct {
				Kind    string          `json:"sessionUpdate"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(update.Update, &event); err != nil {
				t.Fatalf("decode session update: %v", err)
			}
			if event.Kind != "agent_message_chunk" {
				continue
			}
			var content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(event.Content, &content); err != nil {
				t.Fatalf("decode agent message content: %v", err)
			}
			if content.Type == "text" && strings.Contains(content.Text, want) {
				return
			}
		case <-timer.C:
			t.Fatalf("agent did not emit text %q", want)
		}
	}
}

func waitForToolOutput(t *testing.T, updates <-chan acp.SessionUpdate, want string) {
	t.Helper()

	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-updates:
			var event struct {
				Kind    string          `json:"sessionUpdate"`
				Status  string          `json:"status"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(update.Update, &event); err != nil {
				t.Fatalf("decode session update: %v", err)
			}
			if event.Kind == "tool_call_update" &&
				event.Status == "in_progress" &&
				strings.Contains(string(event.Content), want) {
				return
			}
		case <-timer.C:
			t.Fatalf("tool did not emit output %q", want)
		}
	}
}

func startFakeProvider(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("start fake provider listener: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(handleFakeProvider)}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	port := listener.Addr().(*net.TCPAddr).Port
	config := map[string]any{
		"autoupdate":        false,
		"share":             "disabled",
		"model":             "fake/fake-model",
		"enabled_providers": []string{"fake"},
		"mcp":               map[string]any{},
		"provider": map[string]any{
			"fake": map[string]any{
				"npm": "@ai-sdk/openai-compatible",
				"models": map[string]any{
					"fake-model": map[string]any{},
				},
				"options": map[string]any{
					"apiKey":  "not-a-real-secret",
					"baseURL": fmt.Sprintf("http://host.docker.internal:%d/v1", port),
				},
			},
		},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("encode fake provider config: %v", err)
	}
	return string(data)
}

type fakeChatRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func handleFakeProvider(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
		http.NotFound(response, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	var chat fakeChatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}

	texts := make([]string, 0, len(chat.Messages))
	for _, message := range chat.Messages {
		texts = append(texts, message.Role+":"+messageText(message.Content))
	}
	transcript := strings.Join(texts, "\n")
	activePrompt := latestUserMessage(chat)
	if strings.Contains(activePrompt, blockingPrompt) {
		response.Header().Set("Content-Type", "text/event-stream")
		writeSSE(response, map[string]any{
			"id":     "chatcmpl-fake",
			"object": "chat.completion.chunk",
			"choices": []map[string]any{{
				"delta": map[string]string{"content": workingMarker},
			}},
		})
		if flusher, ok := response.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		return
	}
	reply := "UNEXPECTED_PROMPT"
	switch {
	case strings.Contains(transcript, "Generate a title for this conversation"):
		reply = "History continuation test"
	case strings.Contains(activePrompt, continuationPrompt):
		seed := strings.Index(transcript, "user:"+seedPrompt)
		marker := strings.Index(transcript, "assistant:"+firstMarker)
		continuation := strings.LastIndex(transcript, "user:"+continuationPrompt)
		if seed >= 0 && marker > seed && continuation > marker {
			reply = continuedMarker
		} else {
			reply = "HISTORY_MISSING"
		}
	case strings.Contains(activePrompt, recoveryPrompt):
		reply = recoveryMarker
	case strings.Contains(activePrompt, localToolPrompt):
		if !hasFakeTool(chat, "bash") {
			reply = "LOCAL_TOOL_MISSING"
			break
		}
		writeFakeToolCall(response, "bash", map[string]any{
			"command": localToolCommand,
			"timeout": 180_000,
			"workdir": acp.WorkspacePath,
		})
		return
	case strings.Contains(transcript, mcpSideEffect):
		reply = mcpResultMarker
	case strings.Contains(activePrompt, mcpPrompt):
		if !hasFakeTool(chat, "compat_echo") {
			reply = "MCP_TOOL_MISSING"
			break
		}
		writeFakeToolCall(response, "compat_echo", map[string]any{"value": "phase-2"})
		return
	case strings.Contains(activePrompt, seedPrompt):
		reply = firstMarker
	}

	response.Header().Set("Content-Type", "text/event-stream")
	writeSSE(response, map[string]any{
		"id":     "chatcmpl-fake",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta": map[string]string{"role": "assistant"},
		}},
	})
	writeSSE(response, map[string]any{
		"id":     "chatcmpl-fake",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta": map[string]string{"content": reply},
		}},
	})
	writeSSE(response, map[string]any{
		"id":     "chatcmpl-fake",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	_, _ = io.WriteString(response, "data: [DONE]\n\n")
}

func writeSSE(response io.Writer, event any) {
	data, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(response, "data: %s\n\n", data)
}

func writeFakeToolCall(response http.ResponseWriter, name string, arguments map[string]any) {
	encodedArguments, _ := json.Marshal(arguments)
	response.Header().Set("Content-Type", "text/event-stream")
	writeSSE(response, map[string]any{
		"id":     "chatcmpl-fake-tool",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": 0,
					"id":    "call_compat_echo",
					"type":  "function",
					"function": map[string]string{
						"name":      name,
						"arguments": string(encodedArguments),
					},
				}},
			},
		}},
	})
	writeSSE(response, map[string]any{
		"id":     "chatcmpl-fake-tool",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	})
	_, _ = io.WriteString(response, "data: [DONE]\n\n")
}

func hasFakeTool(request fakeChatRequest, name string) bool {
	for _, tool := range request.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func latestUserMessage(request fakeChatRequest) string {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			return messageText(request.Messages[index].Content)
		}
	}
	return ""
}

func messageText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return string(content)
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			result.WriteString(part.Text)
		}
	}
	if result.Len() > 0 {
		return result.String()
	}
	return string(content)
}

type testMCPServer struct {
	URL           string
	blockPoint    mcpBlockPoint
	blockConsumed bool
	callObserved  chan mcpToolCall
	mutex         sync.Mutex
	calls         []mcpToolCall
	initializing  bool
	initialized   bool
}

type mcpBlockPoint uint8

const (
	mcpBlockNone mcpBlockPoint = iota
	mcpBlockBeforeSideEffect
	mcpBlockAfterSideEffect
)

type mcpToolCall struct {
	Name      string
	Arguments map[string]any
}

type mcpRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func startTestMCPServer(t *testing.T) *testMCPServer {
	return startMCPServer(t, mcpBlockNone)
}

func startMCPServer(t *testing.T, blockPoint mcpBlockPoint) *testMCPServer {
	t.Helper()

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("start test MCP listener: %v", err)
	}
	fixture := &testMCPServer{
		blockPoint:   blockPoint,
		callObserved: make(chan mcpToolCall, 1),
	}
	server := &http.Server{Handler: http.HandlerFunc(fixture.handle)}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	port := listener.Addr().(*net.TCPAddr).Port
	fixture.URL = fmt.Sprintf("http://host.docker.internal:%d/mcp", port)
	return fixture
}

func (fixture *testMCPServer) handle(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Origin") != "" {
		http.Error(response, "origin is not allowed", http.StatusForbidden)
		return
	}
	if request.URL.Path != "/mcp" {
		http.NotFound(response, request)
		return
	}
	if request.Method == http.MethodGet {
		if !fixture.ready(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		if !acceptsMediaType(request.Header.Get("Accept"), "text/event-stream") {
			http.Error(response, "Accept must include text/event-stream", http.StatusNotAcceptable)
			return
		}
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.Method == http.MethodDelete {
		if !fixture.ready(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !acceptsMediaType(request.Header.Get("Accept"), "application/json") ||
		!acceptsMediaType(request.Header.Get("Accept"), "text/event-stream") {
		http.Error(response, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	if !hasMediaType(request.Header.Get("Content-Type"), "application/json") {
		http.Error(response, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	var rpcRequest mcpRPCRequest
	if err := json.Unmarshal(body, &rpcRequest); err != nil || rpcRequest.JSONRPC != "2.0" {
		http.Error(response, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}

	switch rpcRequest.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		if json.Unmarshal(rpcRequest.Params, &params) != nil ||
			params.ProtocolVersion == "" ||
			params.Capabilities == nil ||
			params.ClientInfo.Name == "" ||
			params.ClientInfo.Version == "" {
			writeMCPError(response, rpcRequest.ID, -32602, "invalid initialize params")
			return
		}
		fixture.mutex.Lock()
		fixture.initializing = true
		fixture.initialized = false
		fixture.mutex.Unlock()
		writeMCPResult(response, rpcRequest.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]bool{"listChanged": false},
			},
			"serverInfo": map[string]string{
				"name":    "omnigrex-compatibility-fixture",
				"version": "1.0.0",
			},
		})
	case "notifications/initialized":
		if !fixture.completeInitialization(request) {
			http.Error(response, "MCP initialization is invalid", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusAccepted)
	case "tools/list":
		if !fixture.ready(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		writeMCPResult(response, rpcRequest.ID, map[string]any{
			"tools": []map[string]any{{
				"name":        "echo",
				"description": "Records and returns one compatibility side effect",
				"inputSchema": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"value": map[string]string{"type": "string"}},
					"required":             []string{"value"},
					"additionalProperties": false,
				},
			}},
		})
	case "tools/call":
		if !fixture.ready(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(rpcRequest.Params, &params) != nil || params.Name != "echo" {
			writeMCPError(response, rpcRequest.ID, -32602, "invalid tool call")
			return
		}
		call := mcpToolCall{Name: params.Name, Arguments: params.Arguments}
		if fixture.takeBlock(mcpBlockBeforeSideEffect) {
			fixture.observeCall(call)
			<-request.Context().Done()
			return
		}
		fixture.mutex.Lock()
		fixture.calls = append(fixture.calls, call)
		fixture.mutex.Unlock()
		fixture.observeCall(call)
		if fixture.takeBlock(mcpBlockAfterSideEffect) {
			<-request.Context().Done()
			return
		}
		writeMCPResult(response, rpcRequest.ID, map[string]any{
			"content": []map[string]string{{
				"type": "text",
				"text": fmt.Sprintf("%s:%v", mcpSideEffect, params.Arguments["value"]),
			}},
		})
	default:
		if !fixture.ready(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		if len(rpcRequest.ID) == 0 {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		writeMCPError(response, rpcRequest.ID, -32601, "method not found")
	}
}

func (fixture *testMCPServer) observeCall(call mcpToolCall) {
	select {
	case fixture.callObserved <- call:
	default:
	}
}

func (fixture *testMCPServer) takeBlock(point mcpBlockPoint) bool {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if fixture.blockPoint != point || fixture.blockConsumed {
		return false
	}
	fixture.blockConsumed = true
	return true
}

func acceptsMediaType(header, mediaType string) bool {
	for value := range strings.SplitSeq(header, ",") {
		parsed, parameters, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || !strings.EqualFold(parsed, mediaType) {
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

func hasMediaType(header, mediaType string) bool {
	parsed, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(parsed, mediaType)
}

func (fixture *testMCPServer) completeInitialization(request *http.Request) bool {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if !fixture.initializing || request.Header.Get("MCP-Protocol-Version") != mcpProtocolVersion {
		return false
	}
	fixture.initializing = false
	fixture.initialized = true
	return true
}

func (fixture *testMCPServer) ready(request *http.Request) bool {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	return fixture.initialized && request.Header.Get("MCP-Protocol-Version") == mcpProtocolVersion
}

func writeMCPResult(response http.ResponseWriter, id json.RawMessage, result any) {
	writeMCPJSON(response, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeMCPError(response http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeMCPJSON(response, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func writeMCPJSON(response http.ResponseWriter, payload any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(payload)
}

func (fixture *testMCPServer) assertSingleCall(t *testing.T, name string, arguments map[string]any) {
	t.Helper()

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if len(fixture.calls) != 1 {
		t.Fatalf("MCP tool calls = %+v, want exactly one", fixture.calls)
	}
	call := fixture.calls[0]
	if call.Name != name || !mapsEqual(call.Arguments, arguments) {
		t.Fatalf("MCP tool call = %+v, want name %q and arguments %+v", call, name, arguments)
	}
}

func (fixture *testMCPServer) assertNoCalls(t *testing.T) {
	t.Helper()

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if len(fixture.calls) != 0 {
		t.Fatalf("MCP tool calls = %+v, want none", fixture.calls)
	}
}

func (fixture *testMCPServer) waitForCall(t *testing.T) mcpToolCall {
	t.Helper()

	select {
	case call := <-fixture.callObserved:
		return call
	case <-time.After(15 * time.Second):
		t.Fatal("MCP tool was not called")
		return mcpToolCall{}
	}
}

func mapsEqual(left, right map[string]any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func compatibilityPermissionDecision(_ context.Context, request acp.PermissionRequest) acp.PermissionDecision {
	var toolCall struct {
		ID       string         `json:"toolCallId"`
		Title    string         `json:"title"`
		Kind     string         `json:"kind"`
		Status   string         `json:"status"`
		RawInput map[string]any `json:"rawInput"`
	}
	if json.Unmarshal(request.ToolCall, &toolCall) != nil || toolCall.ID == "" || toolCall.Status != "pending" {
		return acp.PermissionDecision{}
	}
	authorized := toolCall.Title == "compat_echo" && toolCall.Kind == "other" && len(toolCall.RawInput) == 0
	if toolCall.Title == localToolCommand &&
		toolCall.Kind == "execute" &&
		toolCall.RawInput["command"] == localToolCommand {
		authorized = true
	}
	if !authorized {
		return acp.PermissionDecision{}
	}
	for _, option := range request.Options {
		if option.Kind == "allow_once" {
			return acp.PermissionDecision{OptionID: option.ID}
		}
	}
	return acp.PermissionDecision{}
}

func (process *openCodeProcess) stop(t *testing.T) {
	process.finish(t, false)
}

func (process *openCodeProcess) kill(t *testing.T) {
	process.finish(t, true)
}

func (process *openCodeProcess) finish(t *testing.T, abrupt bool) {
	t.Helper()

	process.once.Do(func() {
		if abrupt {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := process.process.Stop(stopCtx, 0); err != nil {
				t.Errorf("kill OpenCode Runtime Process: %v", err)
			}
			stopCancel()
		}
		_ = process.client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := process.process.Wait(ctx); err != nil {
			if !abrupt {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = process.process.Stop(stopCtx, 0)
				stopCancel()
				t.Errorf("OpenCode Runtime Process exited with error: %v\nstderr:\n%s", err, process.stderr.String())
			}
		}
		cancel()
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer removeCancel()
		if err := process.process.Remove(removeCtx); err != nil {
			t.Errorf("remove OpenCode Runtime Process: %v", err)
		}
		if err := process.engine.Close(); err != nil {
			t.Errorf("close Docker Engine client: %v", err)
		}
	})
}

func initializeOpenCode(t *testing.T, client *acp.Client) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if response.AgentInfo == nil || response.AgentInfo.Name != "OpenCode" || response.AgentInfo.Version != "1.18.19" {
		t.Errorf("agent info = %+v, want OpenCode 1.18.19", response.AgentInfo)
	}
	if !response.AgentCapabilities.SupportsSessionList() ||
		!response.AgentCapabilities.SupportsSessionLoad() ||
		!response.AgentCapabilities.SupportsSessionResume() {
		t.Errorf("required capabilities are missing: %+v", response.AgentCapabilities)
	}
}

func createDockerVolume(t *testing.T, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "volume", "create", name)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create Docker volume %q: %v\n%s", name, err, output)
	}
	command = exec.CommandContext(
		ctx,
		"docker", "run", "--rm",
		"--mount", "type=volume,src="+name+",dst=/volume",
		"alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
		"sh", "-c", "mkdir -p /volume/assignment && chown 10001:10001 /volume/assignment",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create assignment subpath in Docker volume %q: %v\n%s", name, err, output)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", "volume", "rm", "--force", name)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove Docker volume %q: %v\n%s", name, err, output)
		}
	})
}

func localOpenCodeImage(t *testing.T) string {
	t.Helper()

	command := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", openCodeImageTag)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect OpenCode image %q: %v\n%s", openCodeImageTag, err, output)
	}
	return strings.TrimSpace(string(output))
}

func assertRuntimeStateExcludes(t *testing.T, volume, sentinel string) {
	t.Helper()

	command := exec.Command(
		"docker", "run", "--rm",
		"--mount", "type=volume,src="+volume+",dst=/state,readonly",
		"--entrypoint", "sh",
		"alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
		"-c", "test -f /state/assignment/opencode.db && ! grep -R -F -q -- \"$1\" /state/assignment",
		"sh", sentinel,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("runtime state contains provider credential or lacks database: %v\n%s", err, output)
	}
}

func uniqueDockerName(purpose string) string {
	return strings.ToLower(fmt.Sprintf("omnigrex-acp-%s-%d", purpose, time.Now().UnixNano()))
}
