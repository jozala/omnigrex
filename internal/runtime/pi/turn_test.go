package pi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestPiProcessPreservesApprovedDiskBackedPaths(t *testing.T) {
	p, err := profile.NewPiV1("registry.example/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: "10000000-0000-4000-8000-000000000001", AgentSessionID: "20000000-0000-4000-8000-000000000001", AgentTurnID: "30000000-0000-4000-8000-000000000001", ExecutionEpoch: 1, RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1"}
	paths := map[string]string{
		"TMPDIR":   "/home/opencode/.local/share/omnigrex-tool-data/turn/30000000-0000-4000-8000-000000000001/build-tmp",
		"GOTMPDIR": "/home/opencode/.local/share/omnigrex-tool-data/turn/30000000-0000-4000-8000-000000000001/build-tmp",
		"GOCACHE":  "/home/opencode/.local/share/omnigrex-tool-data/assignment/go-build-cache",
		"GOPATH":   "/home/opencode/.local/share/omnigrex-tool-data/assignment/go-modules",
	}
	adapter := pi.TurnAdapter{}
	request, err := adapter.BuildProcess(p, key, "workflow", 512<<20, paths)
	if err != nil {
		t.Fatalf("valid approved Turn paths rejected: %v", err)
	}
	actual := map[string]string{}
	for _, entry := range request.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatal("malformed environment")
		}
		if _, exists := actual[name]; exists {
			t.Fatalf("duplicate variable: %s", name)
		}
		actual[name] = value
	}
	for name, want := range paths {
		if actual[name] != want {
			t.Fatalf("%s = %q, want %q", name, actual[name], want)
		}
	}
	fallback, err := adapter.BuildProcess(p, key, "workflow", 512<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range fallback.Environment {
		if entry == "TMPDIR=/tmp" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing unconfigured TMPDIR fallback")
	}
	if _, err := adapter.BuildProcess(p, key, "workflow", 512<<20, map[string]string{"HOME": "/workspace/poison"}); !errors.Is(err, execution.ErrInvalidSpec) {
		t.Fatalf("protected runtime variable accepted: %v", err)
	}
}
