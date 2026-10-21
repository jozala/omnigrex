package dirstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type logCollectionSource struct {
	ref     execution.StateRef
	allowed bool
}

func (source *logCollectionSource) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	if ref != source.ref {
		return nil, false, execution.ErrBindingMismatch
	}
	content, _ := json.Marshal(map[string]any{"version": 1, "subpath": "assignment-" + ref.ParticipantID + "/runtime-state"})
	return content, true, nil
}
func (source *logCollectionSource) CollectionState(context.Context, string) (execution.StateRef, error) {
	if !source.allowed {
		return execution.StateRef{}, execution.ErrConflict
	}
	return source.ref, nil
}
func (source *logCollectionSource) ListProtectedStates(context.Context) ([]execution.StateRef, error) {
	return []execution.StateRef{source.ref}, nil
}

func collectionFixture(t *testing.T) (*RetainedState, *logCollectionSource, string) {
	t.Helper()
	root := t.TempDir()
	id := "10000000-0000-4000-8000-000000000001"
	ref := execution.StateRef{ID: id, ParticipantID: id, Backend: execution.BackendBinding{Family: "microsandbox", Version: 1, Namespace: "tests"}, Format: "pi-session/v3", RuntimeProfileSHA256: strings.Repeat("a", 64), Image: "registry.example/pi@sha256:" + strings.Repeat("b", 64)}
	source := &logCollectionSource{ref: ref}
	state, err := New(root, ref.Backend, source)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "assignment-"+id)
	for _, name := range []string{"runtime-state", "runtime-logs"} {
		if err := os.MkdirAll(filepath.Join(parent, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "runtime-state", "session.jsonl"), []byte("session"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "runtime-logs", "exec.log"), []byte("private-native-content"), 0600); err != nil {
		t.Fatal(err)
	}
	return state, source, parent
}

func TestCollectRuntimeLogsRequiresParticipantAuthority(t *testing.T) {
	state, source, parent := collectionFixture(t)
	if err := state.Collect(context.Background(), source.ref); err == nil {
		t.Fatal("unauthorized collection")
	}
	if _, err := os.Stat(filepath.Join(parent, "runtime-logs", "exec.log")); err != nil {
		t.Fatal(err)
	}
}

func TestCollectRuntimeLogsIsIsolatedAndIdempotent(t *testing.T) {
	state, source, parent := collectionFixture(t)
	neighbor := filepath.Join(state.root, "assignment-20000000-0000-4000-8000-000000000001", "runtime-logs")
	if err := os.MkdirAll(neighbor, 0700); err != nil {
		t.Fatal(err)
	}
	source.allowed = true
	// Collection retries must still remove logs if Session removal already succeeded.
	if err := os.RemoveAll(filepath.Join(parent, "runtime-state")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := state.Collect(context.Background(), source.ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "runtime-logs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("logs remain: %v", err)
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(neighbor, filepath.Join(parent, "runtime-logs")); err != nil {
		t.Fatal(err)
	}
	if err := state.Collect(context.Background(), source.ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatal("symlink target deleted", err)
	}
}

func TestSessionExportExcludesRuntimeLogs(t *testing.T) {
	state, source, _ := collectionFixture(t)
	var output bytes.Buffer
	if err := state.Export(context.Background(), source.ref, &output); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("private-native-content")) || bytes.Contains(output.Bytes(), []byte("runtime-logs")) {
		t.Fatal("native logs entered supplemental export")
	}
	if !bytes.Contains(output.Bytes(), []byte("session.jsonl")) {
		t.Fatal("session not exported")
	}
}
