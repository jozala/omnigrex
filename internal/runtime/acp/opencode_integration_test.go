//go:build integration

package acp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
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
)

func TestOpenCodeSessionSurvivesFreshContainers(t *testing.T) {
	image := localOpenCodeImage(t)
	providerConfig := startFakeProvider(t)
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
	session, err := first.client.CreateSession(ctx, acp.CreateSessionRequest{CWD: acp.WorkspacePath})
	cancel()
	if err != nil {
		t.Fatalf("CreateSession() error = %v\nOpenCode stderr:\n%s", err, first.stderr.String())
	}
	prompt(t, first, session.ID, seedPrompt)
	waitForAgentText(t, firstUpdates, firstMarker)
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
		SessionID: session.ID,
		CWD:       acp.WorkspacePath,
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
		SessionID: session.ID,
		CWD:       acp.WorkspacePath,
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
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(update.Update, &event); err != nil {
				t.Fatalf("decode session update: %v", err)
			}
			if event.Kind == "agent_message_chunk" && event.Content.Type == "text" && strings.Contains(event.Content.Text, want) {
				return
			}
		case <-timer.C:
			t.Fatalf("agent did not emit text %q", want)
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
	if strings.Contains(transcript, blockingPrompt) {
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
	case strings.Contains(transcript, continuationPrompt):
		seed := strings.Index(transcript, "user:"+seedPrompt)
		marker := strings.Index(transcript, "assistant:"+firstMarker)
		continuation := strings.LastIndex(transcript, "user:"+continuationPrompt)
		if seed >= 0 && marker > seed && continuation > marker {
			reply = continuedMarker
		} else {
			reply = "HISTORY_MISSING"
		}
	case strings.Contains(transcript, seedPrompt):
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
		return ""
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func (process *openCodeProcess) stop(t *testing.T) {
	t.Helper()

	process.once.Do(func() {
		_ = process.client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := process.process.Wait(ctx); err != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = process.process.Stop(stopCtx, 0)
			stopCancel()
			t.Errorf("OpenCode Runtime Process exited with error: %v\nstderr:\n%s", err, process.stderr.String())
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
