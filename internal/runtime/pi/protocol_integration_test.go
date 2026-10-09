//go:build integration

package pi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

func TestPiControlledProviderProtocol(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_PROTOCOL_INTEGRATION") != "1" {
		t.Skip("run mise run test-pi-integration or test-pi-microsandbox-integration")
	}
	image, platform, settings := diagnosticFixture(t)
	for _, tc := range []struct {
		mode    string
		status  agent.Status
		failure error
	}{
		{"success", agent.Completed, nil}, {"retry", agent.Completed, nil}, {"provider-error", agent.Failed, nil},
		{"lost-terminal", agent.Unknown, context.DeadlineExceeded}, {"hold", agent.Unknown, context.DeadlineExceeded},
		{"cancel", agent.Aborted, nil}, {"process-exit", agent.Unknown, agent.ErrTransport}, {"malformed", agent.Unknown, agent.ErrProtocol},
		{"retry-cancel", agent.Aborted, nil},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			stderr := &diagnosticOutput{}
			spec := pi.DiagnosticSpec(image, platform)
			if tc.mode == "retry-cancel" {
				// Hold Pi's real retry backoff longer than the prompt budget. Abort
				// only after its retry-start event, before another attempt can begin.
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_DIAGNOSTIC_RETRY_DELAY_MS=30000")
			}
			if settings.ExecutionBackend == "microsandbox" {
				spec.Namespace = settings.MicrosandboxNamespace
			} else {
				spec.Namespace = settings.RuntimeStateVolume
			}
			process, owner, err := composition.StartProtocolDiagnostic(ctx, spec, composition.ProbeOptions{}, stderr, settings)
			if err != nil {
				t.Fatal(err)
			}
			c := pi.Connect(process.Transport(), pi.Options{ExpectedSessionID: "omnigrex-pi-diagnostic", CancellationTimeout: time.Second})
			t.Cleanup(func() {
				_ = c.Close()
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				if err := process.Stop(cleanup, 5*time.Second); err != nil {
					t.Errorf("stop: %v", err)
					return
				}
				if err := process.Remove(cleanup); err != nil {
					t.Errorf("remove: %v", err)
				}
				if err := owner.Close(); err != nil {
					t.Errorf("owner close: %v", err)
				}
			})
			if _, err := c.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			promptCtx, stopPrompt := context.WithTimeout(ctx, 10*time.Second)
			if tc.mode == "hold" || tc.mode == "lost-terminal" {
				stopPrompt()
				promptCtx, stopPrompt = context.WithTimeout(ctx, 500*time.Millisecond)
			}
			defer stopPrompt()
			sink := &diagnosticSink{admitted: make(chan struct{}), activity: make(chan struct{}), retry: make(chan struct{})}
			cancelDone := make(chan error, 1)
			if tc.mode == "cancel" || tc.mode == "retry-cancel" {
				cancelAt := sink.activity
				if tc.mode == "retry-cancel" {
					cancelAt = sink.retry
				}
				go func() {
					select {
					case <-cancelAt:
						cancelDone <- c.Cancel(ctx)
					case <-ctx.Done():
						cancelDone <- ctx.Err()
					}
				}()
			}
			got, err := c.Prompt(promptCtx, "protocol-"+tc.mode, sink)
			if tc.mode == "cancel" || tc.mode == "retry-cancel" {
				if err := <-cancelDone; err != nil {
					t.Fatalf("cancel: %v", err)
				}
			}
			if !errors.Is(err, tc.failure) || got.Status != tc.status || !got.Admitted {
				t.Fatalf("outcome = %+v, %v", got, err)
			}
			if tc.mode == "retry-cancel" && got.FailureClass != "" {
				t.Fatalf("abort retained provider failure: %+v", got)
			}
			if tc.mode == "retry-cancel" {
				var terminal struct {
					Event          string `json:"event"`
					Reason         string `json:"assistant_stop_reason"`
					RetryCancelled bool   `json:"retry_cancelled"`
				}
				for _, event := range sink.events {
					if event.Kind == "prompt_settled" {
						if err := json.Unmarshal(event.Metadata.Runtime, &terminal); err != nil {
							t.Fatal(err)
						}
					}
				}
				if terminal.Event != "agent_settled" || terminal.Reason != "error" || !terminal.RetryCancelled {
					t.Fatalf("missing settled retry cancellation evidence: %+v", terminal)
				}
			}
			if strings.Contains(sink.String()+fmt.Sprintf("%+v %#v", got, err), "diagnostic-only-key") {
				t.Fatal("provider credential entered observations")
			}
			if !strings.Contains(stderr.String(), "pi-diagnostic-stderr") {
				t.Fatal("separate stderr missing")
			}
		})
	}
}

func TestPiDiagnosticCommand(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_PROTOCOL_INTEGRATION") != "1" {
		t.Skip("explicit Pi integration gate required")
	}
	image, platform, settings := diagnosticFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	args := []string{"pi-diagnostic", "--image", image, "--arch", platform.Architecture, "--backend", settings.ExecutionBackend}
	if settings.ExecutionBackend == "microsandbox" {
		args = append(args, "--namespace", settings.MicrosandboxNamespace, "--microsandbox-home", settings.MicrosandboxHome, "--microsandbox-executable", settings.MicrosandboxExecutable, "--microsandbox-firmware", settings.MicrosandboxFirmware)
	} else {
		args = append(args, "--namespace", settings.RuntimeStateVolume)
	}
	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"protocol", nil}, {"Developer configuration", []string{"--configuration"}}, {"Reviewer configuration", []string{"--configuration", "--reviewer"}}, {"Session coordination", []string{"--sessions"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var command *exec.Cmd
			arguments := append(append([]string(nil), args...), tc.flags...)
			if settings.ExecutionBackend == "microsandbox" {
				command = exec.CommandContext(ctx, "/usr/local/bin/omnigrex", arguments...)
			} else {
				command = exec.CommandContext(ctx, "go", append([]string{"run", "../../../cmd/omnigrex"}, arguments...)...)
			}
			if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "PASS Pi") {
				t.Fatalf("diagnostic command: %v\n%s", err, output)
			}
		})
	}
}

type diagnosticOutput struct {
	mu      sync.Mutex
	content strings.Builder
}

func (o *diagnosticOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.content.Write(data)
}
func (o *diagnosticOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.content.String()
}

type diagnosticSink struct {
	mu                                    sync.Mutex
	events                                []agentevent.AgentEvent
	admitted, activity, retry             chan struct{}
	onceAdmitted, onceActivity, onceRetry sync.Once
}

func (s *diagnosticSink) Emit(_ context.Context, event agentevent.AgentEvent) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	if event.Kind == "prompt_admitted" {
		s.onceAdmitted.Do(func() { close(s.admitted) })
	}
	if event.Kind == "activity" {
		s.onceActivity.Do(func() { close(s.activity) })
	}
	var diagnostic struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(event.Metadata.Runtime, &diagnostic) == nil && diagnostic.Event == "auto_retry_start" {
		s.onceRetry.Do(func() { close(s.retry) })
	}
	return nil
}
func (s *diagnosticSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%+v", s.events)
}

func diagnosticFixture(t *testing.T) (string, execution.Platform, config.Config) {
	t.Helper()
	if os.Getenv("OMNIGREX_PI_BACKEND") == "microsandbox" {
		return nativeDiagnosticFixture(t)
	}
	output, err := exec.Command("docker", "image", "inspect", "omnigrex/pi-diagnostic:step09", "--format", "{{.Id}} {{.Architecture}}").Output()
	if err != nil {
		t.Fatal("build the disposable Pi image with mise run pi-diagnostic-image")
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		t.Fatal("incomplete diagnostic image identity")
	}
	return fields[0], execution.Platform{OS: "linux", Architecture: fields[1]}, config.Config{ExecutionBackend: "docker", RuntimeStateVolume: "omnigrex-runtime-state"}
}
