//go:build integration && microsandbox && cgo

package microsandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type nativeArchiveStateSource struct{ stateFixtureSource }

func (source *nativeArchiveStateSource) CollectionState(context.Context, string) (execution.StateRef, error) {
	return source.ref, nil
}
func (source *nativeArchiveStateSource) ListProtectedStates(context.Context) ([]execution.StateRef, error) {
	return []execution.StateRef{source.ref}, nil
}

func TestNativeRuntimeLogsSurviveCleanup(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	ref := execution.StateRef{ID: request.Key.AssignmentID, ParticipantID: request.Key.AssignmentID, Backend: backend.Binding(), Format: "pi-session/v3", Image: request.Image, RuntimeProfileSHA256: strings.Repeat("b", 64)}
	source := &nativeArchiveStateSource{stateFixtureSource{ref: ref, initialized: false}}
	state, err := dirstate.New(backend.options.StateRoot, backend.Binding(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backend.options.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := state.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	source.initialized = true
	request.State = ref
	request.State.SessionID = request.Key.AgentSessionID
	request.Attachments = []execution.Attachment{{Resource: "state", Target: "/state"}}
	request.Command = []string{"/bin/sh", "-ec", `test ! -e /state/../runtime-logs; printf ready; exec sleep 300`}
	hostLogs := filepath.Join(backend.options.StateRoot, "assignment-"+request.Key.AssignmentID, "runtime-logs")
	if err := os.MkdirAll(hostLogs, 0700); err != nil {
		t.Fatal(err)
	}
	writeArchiveFile(t, filepath.Join(hostLogs, "host-only"), "private marker")
	process := launchFixture(t, ctx, backend, request)
	var ready [5]byte
	if _, err := io.ReadFull(process.Transport(), ready[:]); err != nil || string(ready[:]) != "ready" {
		t.Fatalf("guest did not prove archive isolation: %q, %v", ready, err)
	}
	if err := backend.EnsureKeyStopped(ctx, request.Key); err != nil {
		t.Fatal(err)
	}
	if err := process.Transport().Close(); err != nil {
		t.Fatal(err)
	}
	meta, err := backend.inspect(ctx, nameFor(request.Key))
	if err != nil {
		t.Fatal(err)
	}
	nativeLogs := filepath.Join(backend.options.Home, "sandboxes", meta.Name, "logs")
	want := map[string][]byte{}
	for _, name := range []string{"runtime.log", "kernel.log", "exec.log"} {
		content, err := os.ReadFile(filepath.Join(nativeLogs, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		t.Logf("native %s exists=%t bytes=%d", name, err == nil, len(content))
		if len(content) == 0 {
			// A host fixture marker proves copying only, not that streamed guest
			// execs are captured by the SDK's primary-execution log.
			content = []byte("host fixture marker for " + name)
			if err := os.WriteFile(filepath.Join(nativeLogs, name), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
		want[name] = content
	}
	// Reconstruct controller-side Backend state from its durable journal.
	recovered := &Backend{options: backend.options, platform: backend.platform, ledger: backend.ledger, inputs: newInputBindings()}
	if err := recovered.EnsureKeyAbsent(ctx, request.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(nativeLogs)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox directory survived removal: %v", err)
	}
	destination := archiveDestination(backend, request.Key, meta.ID)
	for name, expected := range want {
		actual, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("archive %s changed: bytes=%d, %v", name, len(actual), err)
		}
	}
	if err := state.Collect(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hostLogs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authorized collection retained native logs: %v", err)
	}
}
