package pi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestCheckedConfigurationRejectsUnsupportedAndClampedValues(t *testing.T) {
	for _, tc := range []struct {
		name, model, thinking, effective string
		marker                           bool
		wantError                        bool
	}{
		{"exact", "provider/model", "off", "off", true, false},
		{"optional thinking", "provider/model", "", "off", true, false},
		{"model pattern", "provider/mod", "off", "off", true, true},
		{"unsupported thinking", "provider/model", "high", "off", true, true},
		{"clamped thinking", "provider/model", "low", "off", true, true},
		{"missing MCP discovery", "provider/model", "off", "off", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, runtime := net.Pipe()
			connection := pi.Connect(client, pi.Options{Selection: &agent.ModelSelection{Model: tc.model, Thinking: tc.thinking}, RequireMCP: true})
			t.Cleanup(func() { _ = connection.Close(); _ = runtime.Close() })
			go func() {
				scanner := bufio.NewScanner(runtime)
				for scanner.Scan() {
					var command struct{ ID, Type string }
					_ = json.Unmarshal(scanner.Bytes(), &command)
					var data any
					switch command.Type {
					case "get_state":
						data = map[string]any{"sessionId": "fixture", "isStreaming": false, "model": map[string]string{"provider": "provider", "id": "model"}, "thinkingLevel": tc.effective}
					case "get_commands":
						commands := []any{}
						if tc.marker {
							commands = append(commands, map[string]string{"name": "omnigrex-ready", "source": "extension"})
						}
						data = map[string]any{"commands": commands}
					case "get_available_models":
						data = map[string]any{"models": []any{map[string]string{"provider": "provider", "id": "model"}}}
					case "get_available_thinking_levels":
						data = map[string]any{"levels": []string{"off", "low"}}
					case "prompt":
						t.Error("prompt admitted despite failed configuration")
						return
					}
					writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": data})
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := connection.Prepare(ctx)
			if errors.Is(err, pi.ErrConfiguration) != tc.wantError || !tc.wantError && err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if tc.wantError {
				if outcome, err := connection.Prompt(ctx, "must not admit", nil); err == nil || outcome.Admitted {
					t.Fatalf("prompt after failed configuration: %v", err)
				}
			}
		})
	}
}

func TestEphemeralConfigurationSelectsOnlyRequestedProvider(t *testing.T) {
	configuration, err := pi.ConfigurationDiagnostic(role.Reviewer)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(configuration)
	if strings.Contains(string(encoded), "must-not-be-delivered") || strings.Contains(string(encoded), "unrelated") {
		t.Fatal("unrelated provider delivered")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", configuration, configuration), "configuration-only-key") {
		t.Fatal("configuration debug formatting leaked key")
	}
	client, runtime := net.Pipe()
	defer func() { _ = runtime.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	delivered := make(chan []byte, 1)
	go func() {
		scanner := bufio.NewScanner(runtime)
		if scanner.Scan() {
			delivered <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	connection, err := pi.Start(ctx, client, configuration, pi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if string(<-delivered) != string(encoded) {
		t.Fatal("bootstrap changed exact configuration")
	}
}

func TestBootstrapWriteIsBounded(t *testing.T) {
	configuration, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	client, runtime := net.Pipe()
	defer func() { _ = runtime.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := pi.Start(ctx, client, configuration, pi.Options{CloseTimeout: time.Millisecond}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bootstrap: %v", err)
	}
}

func TestStartupSharesBudgetAcrossConfigurationAndFirstState(t *testing.T) {
	configuration, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		startup  time.Duration
		parent   time.Duration
		wantFail bool
	}{
		{"startup permits slow bootstrap and readiness", 2 * time.Second, 3 * time.Second, false},
		{"one shared startup budget", 150 * time.Millisecond, 3 * time.Second, true},
		{"shorter parent wins", 2 * time.Second, 150 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, runtime := net.Pipe()
			defer func() { _ = runtime.Close() }()
			go func() {
				time.Sleep(100 * time.Millisecond)
				scanner := bufio.NewScanner(runtime)
				if !scanner.Scan() {
					return
				}
				if !scanner.Scan() {
					return
				}
				var command struct{ ID, Type string }
				_ = json.Unmarshal(scanner.Bytes(), &command)
				time.Sleep(100 * time.Millisecond)
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "fixture", "isStreaming": false}})
				for scanner.Scan() {
					_ = json.Unmarshal(scanner.Bytes(), &command)
					var data any
					switch command.Type {
					case "get_commands":
						data = map[string]any{"commands": []any{map[string]string{"name": "omnigrex-ready", "source": "extension"}}}
					case "get_available_models":
						data = map[string]any{"models": []any{map[string]string{"provider": "diagnostic", "id": "controlled"}}}
					case "get_available_thinking_levels":
						data = map[string]any{"levels": []string{"off"}}
					case "get_state":
						data = map[string]any{"sessionId": "fixture", "isStreaming": false, "model": map[string]string{"provider": "diagnostic", "id": "controlled"}, "thinkingLevel": "off"}
					}
					writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": data})
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), tc.parent)
			defer cancel()
			connection, err := pi.Start(ctx, client, configuration, pi.Options{RequestTimeout: 30 * time.Millisecond, StartupTimeout: tc.startup, CloseTimeout: time.Millisecond})
			if err == nil {
				defer func() { _ = connection.Close() }()
				_, err = connection.Prepare(ctx)
			}
			if tc.wantFail {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("want deadline, got %v", err)
				}
				if strings.Contains(err.Error(), "Pi startup readiness deadline exceeded") != (tc.startup < tc.parent) {
					t.Fatalf("wrong deadline source: %v", err)
				}
			} else if err != nil {
				t.Fatalf("startup within budget: %v", err)
			}
		})
	}
}

func TestStartupCancellationClosesBlockedConfigurationWrite(t *testing.T) {
	configuration, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	client, runtime := net.Pipe()
	defer func() { _ = runtime.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := pi.Start(ctx, client, configuration, pi.Options{StartupTimeout: time.Second, CloseTimeout: time.Millisecond})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup: %v", err)
	}
}

func TestStartupConfigurationFailureInterruptsBlockedWrite(t *testing.T) {
	configuration, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	client, runtime := net.Pipe()
	blocked := slowClose{client, make(chan struct{})}
	t.Cleanup(func() { close(blocked.release); _ = runtime.Close() })
	go writeRecord(runtime, map[string]any{"type": "omnigrex_configuration_error", "error": "never log guest text"})
	start := time.Now()
	_, err = pi.Start(t.Context(), blocked, configuration, pi.Options{StartupTimeout: 2 * time.Second, CloseTimeout: 10 * time.Millisecond})
	if !errors.Is(err, pi.ErrConfiguration) || time.Since(start) > time.Second {
		t.Fatalf("blocked startup did not return known failure promptly: %v", err)
	}
}

func TestBootstrapPreservesLargeEscapedInstructionsWithinRecordBudget(t *testing.T) {
	configuration, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Instructions = strings.Repeat("<>&", 80000)
	client, runtime := net.Pipe()
	defer func() { _ = runtime.Close() }()
	delivered := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(runtime)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		if scanner.Scan() {
			var received struct {
				Instructions string `json:"instructions"`
			}
			_ = json.Unmarshal(scanner.Bytes(), &received)
			delivered <- received.Instructions
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c, err := pi.Start(ctx, client, configuration, pi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	select {
	case got := <-delivered:
		if got != configuration.Instructions {
			t.Fatal("bootstrap changed instructions")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestOwnedRunnerReportsSafeConfigurationFailure(t *testing.T) {
	client, runtime := net.Pipe()
	connection := pi.Connect(client, pi.Options{})
	t.Cleanup(func() { _ = connection.Close(); _ = runtime.Close() })
	go func() {
		writeRecord(runtime, map[string]any{"type": "omnigrex_configuration_error", "error": "secret must not escape"})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := connection.Prepare(ctx)
	if !errors.Is(err, pi.ErrConfiguration) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "secret") {
		t.Fatalf("configuration failure: %v", err)
	}
}

func TestRenderUsesCurrentDeploymentInstructionsAndLiteralAPIKeys(t *testing.T) {
	p, err := agentprofile.Parse(".omnigrex/team/developer.md", []byte("---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v1\nmodel: openai/o4-mini\npermissions:\n  read: allow\n  skill: allow\n---\nREPOSITORY_PERSONALITY\n"), role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := role.BuiltinPolicyCatalog().Lookup(role.Developer)
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	stage, _ := definition.Stage(workflow.StageImplementation)
	fixture, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, version := range []string{"FIRST_DEPLOYMENT", "CURRENT_DEPLOYMENT"} {
		if err := os.WriteFile(filepath.Join(dir, "common.md"), []byte(version), 0o600); err != nil {
			t.Fatal(err)
		}
		operator, err := agentinstructions.Load(dir, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatal(err)
		}
		got, err := pi.Render(p, policy, operator, stage, workflow.TurnPurposeRequestedChanges, fixture.MCP, []byte(`{"openai":{"type":"api_key","key":"literal-secret"}}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{version, "REPOSITORY_PERSONALITY", "changes since the previous review", "request_review", "Read the repository root `AGENTS.md` explicitly"} {
			if !strings.Contains(got.Instructions, text) {
				t.Fatalf("missing %q", text)
			}
		}
		if version == "CURRENT_DEPLOYMENT" && strings.Contains(got.Instructions, "FIRST_DEPLOYMENT") {
			t.Fatal("historical instructions replayed")
		}
	}
	for _, credentials := range []string{`{}`, `{"openai":{"type":"oauth","key":"secret"}}`, `{"openai":{"type":"api_key","key":"!secret-command"}}`, `{"openai":{"type":"api_key","key":""}}`, `{"openai":{"type":"api_key","key":" secret "}}`, `{"openai":{"type":"api_key","key":"$HOME"}}`, `{"openai":{"type":"api_key","key":"${HOME}"}}`, `{"openai":{"type":"api_key","key":"prefix-$HOME"}}`, "not-json-secret"} {
		_, err := pi.Render(p, policy, agentinstructions.Operator{}, stage, workflow.TurnPurposeInitialDevelopment, fixture.MCP, []byte(credentials))
		if !errors.Is(err, pi.ErrConfiguration) || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "secret") {
			t.Fatalf("unsafe credential failure: %v", err)
		}
	}
}
