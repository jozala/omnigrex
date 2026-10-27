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
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/stepbudget"
	mobyclient "github.com/moby/moby/client"
)

func TestPiDurableSessionsAcrossFreshEnvironments(t *testing.T) {
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
		volume := "omnigrex-pi-sessions-" + uuid.NewString()
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
	for _, roleID := range []role.ID{role.Developer, role.Reviewer} {
		t.Run(string(roleID), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			participant, sessionID := uuid.NewString(), uuid.NewString()
			prepare(participant)
			inputs, err := pi.ConfigurationDiagnostic(roleID)
			if err != nil {
				t.Fatal(err)
			}
			inputs.StepBudget = &stepbudget.Limit{}
			start := func(epoch int64, mustExist bool) (agent.Connection, func()) {
				t.Helper()
				key := execution.Key{AssignmentID: participant, AgentSessionID: sessionID, AgentTurnID: uuid.NewString(), ExecutionEpoch: epoch, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2"}
				request := execution.Request{Key: key, Image: image, Platform: platform, User: "10001:10001", WorkingDir: "/workspace", Command: []string{"/usr/local/bin/node", "/opt/omnigrex/pi/configuration.mjs"}, Environment: pi.DiagnosticSpec(image, platform).Environment, Attachments: []execution.Attachment{{Resource: "state", Target: "/state"}}, TemporaryDirectories: pi.DiagnosticSpec(image, platform).TemporaryDirectories, MemoryBytes: 512 << 20, PIDsLimit: 256}
				if epoch == 3 {
					request.Environment = append(request.Environment, "OMNIGREX_PI_HISTORY_TREE_FIXTURE=1")
				}
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
			// Ignore initial preparation response and remove the entire environment.
			first, removeFirst := start(1, false)
			if _, err := first.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			removeFirst()
			recovered, removeRecovered := start(2, false)
			ready, err := recovered.Prepare(ctx)
			if err != nil || ready.RuntimeSessionID != sessionID || ready.Disposition != "recovered" {
				t.Fatalf("exact creation recovery: %+v %v", ready, err)
			}
			outcome, err := recovered.Prompt(ctx, "configuration-long", nil)
			if err != nil || outcome.Status != agent.Completed {
				t.Fatalf("multi-megabyte normal completion: %+v %v", outcome, err)
			}
			outcome, err = recovered.Prompt(ctx, "configuration-audit", nil)
			if err != nil || outcome.Status != agent.Completed || outcome.StepBudget == nil || outcome.StepBudget.Used != 1 || !outcome.StepBudget.Complete || outcome.StepBudget.Summary || outcome.StepBudget.Limit.Finite() {
				t.Fatalf("first prompt: %+v evidence=%+v %v", outcome, outcome.StepBudget, err)
			}
			removeRecovered()
			continued, removeContinued := start(3, true)
			ready, err = continued.Prepare(ctx)
			if err != nil || ready.Disposition != "continued" || ready.RuntimeSessionID != sessionID {
				t.Fatalf("continuation: %+v %v", ready, err)
			}
			sink := &historySink{}
			cursor, err := continued.Replay(ctx, agent.HistoryCursor{}, sink)
			if err != nil || cursor.RuntimeSessionID != sessionID || len(sink.events) < 2 {
				t.Fatalf("persisted replay: %+v %v", cursor, err)
			}
			for _, event := range sink.events {
				if event.AgentSessionID != sessionID || event.AssignmentID != participant {
					t.Fatal("replay crossed Participant ownership")
				}
			}
			users, compactions, branches := 0, 0, 0
			for _, event := range sink.events {
				switch event.Kind {
				case "history_user_message":
					users++
				case "history_compaction":
					compactions++
				case "history_branch_summary":
					branches++
				}
			}
			if users != 263 || compactions != 1 || branches != 1 {
				t.Fatalf("replay omitted append-order branches/compaction: users=%d compactions=%d branches=%d", users, compactions, branches)
			}
			before := len(sink.events)
			if got, err := continued.Replay(ctx, cursor, sink); err != nil || got != cursor || len(sink.events) != before {
				t.Fatalf("cursor replay: %+v %v", got, err)
			}
			outcome, err = continued.Prompt(ctx, "configuration-continuation", nil)
			if err != nil || outcome.Status != agent.Completed || outcome.StepBudget == nil || outcome.StepBudget.Used != 1 || !outcome.StepBudget.Complete || outcome.StepBudget.Summary || outcome.StepBudget.Limit.Finite() {
				t.Fatalf("continued context or provider-free replay: %+v %v", outcome, err)
			}
			_, err = continued.Replay(ctx, agent.HistoryCursor{RuntimeSessionID: uuid.NewString()}, sink)
			if !errors.Is(err, agent.ErrSession) {
				t.Fatalf("foreign cursor: %v", err)
			}
			if strings.Contains(fmt.Sprint(sink.events), "configuration-only-key") {
				t.Fatal("credential in history events")
			}
			removeContinued()
			// An activated unknown exact identity must fail rather than attaching
			// the Participant's latest file or creating a replacement conversation.
			sessionID = uuid.NewString()
			missing, removeMissing := start(4, true)
			if _, err := missing.Prepare(ctx); !errors.Is(err, pi.ErrConfiguration) {
				t.Fatalf("missing retained Session was recreated: %v", err)
			}
			if _, err := missing.Prompt(ctx, "configuration-audit", nil); err == nil {
				t.Fatal("missing retained Session admitted prompt")
			}
			removeMissing()
		})
	}
}
