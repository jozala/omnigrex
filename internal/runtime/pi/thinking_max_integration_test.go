//go:build integration

package pi_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	mobyclient "github.com/moby/moby/client"
)

// TestPiMaxThinkingUnsupportedViaPinnedSDK exercises unsupported selections
// through the actual pinned Pi SDK (not the net.Pipe protocol fixture): the
// fixture controlled-max model advertises low/high/max and excludes xhigh,
// while the non-reasoning controlled model advertises only off.
func TestPiMaxThinkingUnsupportedViaPinnedSDK(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_PROTOCOL_INTEGRATION") != "1" {
		t.Skip("explicit both-backend Pi gate required")
	}
	image, platform, settings := diagnosticFixture(t)
	for _, tc := range []struct{ name, model, thinking string }{
		{"xhigh excluded on controlled-max", "diagnostic/controlled-max", "xhigh"},
		{"max unsupported on non-reasoning controlled", "diagnostic/controlled", "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			inputs, err := pi.ConfigurationDiagnostic(role.Developer)
			if err != nil {
				t.Fatal(err)
			}
			inputs.Selection.Model = tc.model
			inputs.Selection.Thinking = tc.thinking
			spec := pi.ConfigurationDiagnosticSpec(image, platform)
			if settings.ExecutionBackend == "microsandbox" {
				spec.Namespace = settings.MicrosandboxNamespace
			} else {
				spec.Namespace = settings.RuntimeStateVolume
			}
			stderr := &diagnosticOutput{}
			process, owner, err := composition.StartProtocolDiagnostic(ctx, spec, composition.ProbeOptions{}, stderr, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				if err := process.Stop(cleanup, 5*time.Second); err != nil {
					t.Error(err)
					return
				}
				if err := process.Remove(cleanup); err != nil {
					t.Error(err)
				}
				if err := owner.Close(); err != nil {
					t.Error(err)
				}
			})
			c, err := pi.Start(ctx, process.Transport(), inputs, pi.Options{ExpectedSessionID: "omnigrex-pi-configuration", RequestTimeout: 20 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := c.Prepare(ctx); !errors.Is(err, pi.ErrConfiguration) {
				t.Fatalf("unsupported max selection prepared: %v; stderr: %s", err, stderr.String())
			}
			if _, err := c.Prompt(ctx, "must-not-admit", nil); err == nil {
				t.Fatal("prompt after unsupported max selection accepted")
			}
		})
	}
}

// TestPiMaxThinkingLifecycleAcrossFreshEnvironments drives max end to end
// through the pinned Pi SDK: Prepare/Prompt at exactly max with the provider
// request asserting the mapped reasoning setting, real Pi-generated
// thinking_level_change history surviving Execution Environment recreation,
// provider-free History Replay, and a continued prompt at exactly max.
// It runs on both backends via diagnosticFixture.
func TestPiMaxThinkingLifecycleAcrossFreshEnvironments(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_PROTOCOL_INTEGRATION") != "1" {
		t.Skip("explicit both-backend Pi gate required")
	}
	image, platform, settings := diagnosticFixture(t)
	var factory execution.Factory
	prepare := func(string) {}
	if settings.ExecutionBackend == "microsandbox" {
		factory, prepare = nativeSessionFactory(t, settings)
	} else {
		api, err := mobyclient.New(mobyclient.FromEnv, mobyclient.WithAPIVersionNegotiation())
		if err != nil {
			t.Fatal(err)
		}
		volume := "omnigrex-pi-max-thinking-" + uuid.NewString()
		if _, err := api.VolumeCreate(t.Context(), mobyclient.VolumeCreateOptions{Name: volume}); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--user", "0:0", "--network", "none", "--mount", "type=volume,src="+volume+",dst=/state", "--entrypoint", "/bin/sh", image, "-ec", "chown 10001:10001 /state; chmod 0700 /state").CombinedOutput(); err != nil {
			t.Fatalf("initialize disposable state volume: %v %s", err, output)
		}
		t.Cleanup(func() {
			_, _ = api.VolumeRemove(context.Background(), volume, mobyclient.VolumeRemoveOptions{Force: true})
			_ = api.Close()
		})
		factory = docker.ExecutionFactory{Network: "none", WorkspaceVolume: volume, RuntimeStateVolume: volume, MiseVolume: volume}
	}
	ledger := conformance.NewLedger()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	participant, sessionID := uuid.NewString(), uuid.NewString()
	prepare(participant)
	inputs, err := pi.ConfigurationDiagnostic(role.Developer)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture-only selection: the controlled-max reasoning model advertises
	// max through the pinned SDK's thinkingLevelMap. Production never maps
	// models to levels; the live runtime is always queried.
	inputs.Selection.Model = "diagnostic/controlled-max"
	inputs.Selection.Thinking = "max"
	start := func(epoch int64, mustExist bool) (agent.Connection, func()) {
		t.Helper()
		key := execution.Key{AssignmentID: participant, AgentSessionID: sessionID, AgentTurnID: uuid.NewString(), ExecutionEpoch: epoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
		request := execution.Request{Key: key, Image: image, Platform: platform, User: "10001:10001", WorkingDir: "/workspace", Command: []string{"/usr/local/bin/node", "/opt/omnigrex/pi/configuration.mjs"}, Environment: pi.DiagnosticSpec(image, platform).Environment, Attachments: []execution.Attachment{{Resource: "state", Target: "/state"}}, TemporaryDirectories: pi.DiagnosticSpec(image, platform).TemporaryDirectories, MemoryBytes: 512 << 20, PIDsLimit: 256}
		engine, err := factory.New(request, ledger)
		if err != nil {
			t.Fatal(err)
		}
		stderr := &diagnosticOutput{}
		process, err := engine.Create(ctx, request, stderr)
		if err != nil {
			_ = engine.Close()
			t.Fatal(err)
		}
		if err := process.Start(ctx); err != nil {
			t.Fatal(err)
		}
		configuration := inputs
		configuration.Session = &agent.SessionIdentity{AgentSessionID: sessionID, RuntimeSessionID: sessionID, MustExist: mustExist}
		connection, err := pi.Start(ctx, process.Transport(), configuration, pi.Options{EventContext: agentevent.Context{AssignmentID: participant, AgentSessionID: sessionID, TurnID: key.AgentTurnID, ExecutionEpoch: uint64(epoch)}, RequestTimeout: 20 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		cleaned := false
		cleanup := func() {
			if cleaned {
				return
			}
			cleaned = true
			_ = connection.Close()
			cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			if err := process.Stop(cleanup, 5*time.Second); err != nil {
				t.Error(err)
				return
			}
			if err := process.Remove(cleanup); err != nil {
				t.Error(err)
			}
			if err := engine.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Cleanup(cleanup)
		return connection, cleanup
	}
	// Ignore the initial preparation response and remove the entire environment.
	first, removeFirst := start(1, false)
	if _, err := first.Prepare(ctx); err != nil {
		t.Fatalf("max preparation: %v", err)
	}
	removeFirst()
	recovered, removeRecovered := start(2, false)
	ready, err := recovered.Prepare(ctx)
	if err != nil || ready.RuntimeSessionID != sessionID || ready.Disposition != "recovered" {
		t.Fatalf("max creation recovery: %+v %v", ready, err)
	}
	outcome, err := recovered.Prompt(ctx, "configuration-thinking-max", nil)
	if err != nil || outcome.Status != agent.Completed {
		t.Fatalf("max prompt (provider reasoning assertion): %+v %v", outcome, err)
	}
	removeRecovered()
	continued, removeContinued := start(3, true)
	ready, err = continued.Prepare(ctx)
	if err != nil || ready.Disposition != "continued" || ready.RuntimeSessionID != sessionID {
		t.Fatalf("max continuation: %+v %v", ready, err)
	}
	sink := &historySink{}
	cursor, err := continued.Replay(ctx, agent.HistoryCursor{}, sink)
	if err != nil || cursor.RuntimeSessionID != sessionID || len(sink.events) < 2 {
		t.Fatalf("max persisted replay: %+v %v", cursor, err)
	}
	for _, event := range sink.events {
		if event.AgentSessionID != sessionID || event.AssignmentID != participant {
			t.Fatal("max replay crossed Participant ownership")
		}
	}
	// Real Pi-generated thinking history must survive recreation: Prepare at
	// max appends thinking_level_change, and reopening preserves it exactly.
	thinking, users := 0, 0
	for _, event := range sink.events {
		switch event.Kind {
		case "history_thinking_level_change":
			thinking++
		case "history_user_message":
			users++
		}
	}
	if thinking < 1 || users < 1 {
		t.Fatalf("max replay omitted Pi-generated thinking history: thinking=%d users=%d events=%d", thinking, users, len(sink.events))
	}
	before := len(sink.events)
	if got, err := continued.Replay(ctx, cursor, sink); err != nil || got != cursor || len(sink.events) != before {
		t.Fatalf("max cursor replay: %+v %v", got, err)
	}
	outcome, err = continued.Prompt(ctx, "configuration-thinking-max-continue", nil)
	if err != nil || outcome.Status != agent.Completed {
		t.Fatalf("max continued context with provider-free replay: %+v %v", outcome, err)
	}
	_, err = continued.Replay(ctx, agent.HistoryCursor{RuntimeSessionID: uuid.NewString()}, sink)
	if !errors.Is(err, agent.ErrSession) {
		t.Fatalf("foreign cursor: %v", err)
	}
	if strings.Contains(fmt.Sprint(sink.events), "configuration-only-key") {
		t.Fatal("credential in history events")
	}
	removeContinued()
}
