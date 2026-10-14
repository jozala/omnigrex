//go:build integration && microsandbox && cgo

package microsandbox

import (
	"context"
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExecutionConformanceMicrosandbox(t *testing.T) {
	if os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("set OMNIGREX_MICROSANDBOX_INTEGRATION=1 on a qualified Linux/KVM host")
	}
	root, err := os.MkdirTemp("/tmp", "msb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	options := Options{Executable: "/usr/local/bin/msb", Firmware: "/usr/local/lib/libkrunfw.so.5.6.1", Home: "/tmp/msb-step08", Namespace: "conformance", WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "states"), ToolRoot: filepath.Join(root, "tools"), CPUs: 1, MaximumDuration: 5 * time.Minute, RootDiskMiB: 4096}
	ledger := conformance.NewLedger()
	backend, err := New(options, ledger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := backend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	request := execution.Request{Key: execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 1, RuntimeProfileName: "protocol", RuntimeProfileVersion: "v1"}, Image: "docker.io/library/alpine@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412", Platform: execution.Platform{OS: "linux", Architecture: "amd64"}, User: "10001:10001", WorkingDir: "/tmp", MemoryBytes: 512 << 20, PIDsLimit: 128, Command: []string{"/bin/sh", "-c", `printf '{"ready":true}\n'; printf 'stderr-marker\n' >&2; sleep 300 & while IFS= read -r line; do printf '%s\n' "$line"; done; wait`}}
	if err := (commandCLI{options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	conformance.Transport(t, ctx, backend, request, ledger)
}
