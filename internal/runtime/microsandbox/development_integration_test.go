//go:build integration && microsandbox && cgo

package microsandbox

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestMicrosandboxDevelopmentEnvironment(t *testing.T) {
	image := os.Getenv("OMNIGREX_DEVELOPMENT_IMAGE")
	if image == "" || os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("run mise run test-development-image-microsandbox with a published digest on Linux/KVM")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Minute)
	defer cancel()
	root := t.TempDir()
	options := Options{
		Executable: "/usr/local/bin/msb", Firmware: "/usr/local/lib/libkrunfw.so.5.6.1",
		Home: "/tmp/msb-development", Namespace: "development-qualification",
		WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "states"),
		ToolRoot: filepath.Join(root, "tools"), CPUs: 2, MaximumDuration: 55 * time.Minute, RootDiskMiB: 40960,
	}
	backend, err := New(options, conformance.NewLedger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (commandCLI{options}).Run(ctx, nil, os.Stderr, "image", "pull", image); err != nil {
		t.Fatal(err)
	}
	runtime, err := profile.NewPiV2(image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.CheckRepositoryEnvironment(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	key := testRequest().Key
	key.RuntimeProfileName, key.RuntimeProfileVersion = profile.PiRuntimeName, profile.PiRuntimeVersion
	key.AgentTurnID = fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
	workspace := backend.location(key, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "cp", "-R", "/fixtures/development/.", workspace).CombinedOutput(); err != nil {
		t.Fatalf("prepare development checkout: %v\n%s", err, output)
	}
	ref := execution.StateRef{ID: key.AssignmentID, ParticipantID: key.AssignmentID, SessionID: key.AgentSessionID,
		Backend: backend.Binding(), Format: "pi-session/v3", RuntimeProfileSHA256: runtime.ContentSHA256(), Image: image}
	if err := os.MkdirAll(options.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	states, err := dirstate.New(options.StateRoot, backend.Binding(), &stateFixtureSource{ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	toolData := "/home/omnigrex/.local/share/omnigrex-tool-data"
	for turn := 1; turn <= 2; turn++ {
		key.ExecutionEpoch = int64(turn)
		tools := map[string]string{"GOPATH": toolData + "/go-modules", "GOCACHE": toolData + "/go-build",
			"TMPDIR": toolData + fmt.Sprintf("/turn-%d", turn), "GOTMPDIR": toolData + fmt.Sprintf("/turn-%d", turn)}
		for _, path := range tools {
			if err := os.MkdirAll(filepath.Join(backend.location(key, "tool-data"), strings.TrimPrefix(path, toolData+"/")), 0700); err != nil {
				t.Fatal(err)
			}
		}
		request, err := (pi.TurnAdapter{}).BuildProcess(runtime, key, "", 4<<30, tools)
		if err != nil {
			t.Fatal(err)
		}
		request.GoldenResources = true
		request.Requirements = []execution.Requirement{execution.HardwareIsolation, execution.NestedContainers, execution.ToolInstallation}
		request.Workspace = execution.WorkspaceRef{Key: key, Seed: strings.Repeat("a", 40)}
		request.State = ref
		// Open the real durable Pi Session after the checkout checks. The second
		// environment must continue it, while both tool and module caches survive.
		sessionScript := fmt.Sprintf(`
import { openDurableSession } from '/opt/omnigrex/pi/session-state.mjs';
const { disposition } = await openDurableSession({agentSessionId: %q, runtimeSessionId: %q, mustExist: %t});
console.log(disposition);
setInterval(() => {}, 1000);
`, key.AgentSessionID, key.AgentSessionID, turn > 1)
		request.Command = []string{environment.Executable, "run", "/bin/sh", "-ec",
			fmt.Sprintf("sh scripts/qualify-development-guest.sh %d >&2; exec /usr/local/bin/node --input-type=module -e %s", turn, quote(sessionScript))}
		request.Environment = environment.BindEnvironment(request.Environment)
		engine, err := backend.New(request, backend.ledger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer engine.Close()
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := engine.EnsureAbsent(cleanup); err != nil {
				t.Error(err)
			}
		})
		process, err := engine.Create(ctx, request, os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Transport().Close() })
		if err := process.Start(ctx); err != nil {
			t.Fatal(err)
		}
		interrupt := context.AfterFunc(ctx, func() { _ = process.Transport().Close() })
		line, err := bufio.NewReader(process.Transport()).ReadString('\n')
		interrupt()
		want := "created\n"
		if turn > 1 {
			want = "continued\n"
		}
		if err != nil || line != want {
			t.Fatalf("development Turn %d = %q, %v; want %q", turn, line, err, want)
		}
		if err := process.Stop(ctx, 15*time.Second); err != nil {
			t.Fatal(err)
		}
		meta, err := backend.inspect(ctx, nameFor(key))
		if err != nil || meta.Status != "Stopped" {
			t.Fatalf("development environment survived stop: %+v, %v", meta, err)
		}
		if err := process.Remove(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise ordinary Pi initialization and authenticated diagnostic MCP using
	// the same image. Unrelated fixture secret/database/GitHub checks may fail.
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.To4() == nil || ip.IsLoopback() {
			continue
		}
		command := exec.CommandContext(ctx, "/usr/local/bin/omnigrex", "doctor", "--repository", "fixture/repository")
		command.Env = append(os.Environ(), "OMNIGREX_PI_IMAGE="+image, "OMNIGREX_PI_PLATFORM=linux/amd64",
			"OMNIGREX_GITHUB_DEVELOPER_APP_ID=1", "OMNIGREX_GITHUB_REVIEWER_APP_ID=2",
			"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE=/dev/null", "OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE=/dev/null",
			"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE=/dev/null", "OMNIGREX_PROVIDER_CREDENTIALS_FILE=/dev/null",
			"OMNIGREX_DATABASE_URL=postgres://fixture@127.0.0.1:1/fixture?sslmode=disable",
			"OMNIGREX_MICROSANDBOX_HOME="+options.Home, "OMNIGREX_MICROSANDBOX_NAMESPACE="+options.Namespace,
			"OMNIGREX_WORKSPACE_ROOT="+options.WorkspaceRoot, "OMNIGREX_RUNTIME_STATE_ROOT="+options.StateRoot,
			"OMNIGREX_MISE_ROOT="+options.ToolRoot, "OMNIGREX_READINESS_TIMEOUT=60s", "OMNIGREX_MCP_ENDPOINT_URL=http://"+ip.String()+":1/mcp")
		output, _ := command.CombinedOutput()
		if !strings.Contains(string(output), "PASS pi-runtime") {
			t.Fatalf("development image Pi readiness failed:\n%s", output)
		}
		t.Log("PASS: exact development image, golden resources, guest Docker, repository checks/integration, persistent module cache, Pi Session Continuation, cleanup, and authenticated Pi readiness")
		return
	}
	t.Fatal("no reachable host IPv4 address for Pi diagnostic")
}
