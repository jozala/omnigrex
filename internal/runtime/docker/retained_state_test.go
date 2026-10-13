package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type retainedStateFixtureSource struct {
	refs            map[string]execution.StateRef
	initialized     map[string]bool
	collectable     map[string]bool
	metadataVersion int
}

func newRetainedStateFixture() *retainedStateFixtureSource {
	return &retainedStateFixtureSource{refs: make(map[string]execution.StateRef), initialized: make(map[string]bool), collectable: make(map[string]bool), metadataVersion: 1}
}

func (source *retainedStateFixtureSource) add(id, namespace string) execution.StateRef {
	ref := execution.StateRef{ID: id, ParticipantID: id, Backend: execution.BackendBinding{Family: "docker", Version: 1, Namespace: namespace}, Format: "pi-session/v3", Image: "registry.example/pi@sha256:" + strings.Repeat("a", 64), RuntimeProfileSHA256: strings.Repeat("b", 64)}
	source.refs[id] = ref
	return ref
}

func (source *retainedStateFixtureSource) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	actual, found := source.refs[ref.ID]
	actual.SessionID = ref.SessionID
	if !found || actual != ref {
		return nil, false, execution.ErrBindingMismatch
	}
	content, _ := json.Marshal(map[string]any{"version": source.metadataVersion, "subpath": "assignment-" + ref.ParticipantID + "/runtime-state"})
	return content, source.initialized[ref.ID], nil
}

func (source *retainedStateFixtureSource) CollectionState(_ context.Context, id string) (execution.StateRef, error) {
	if !source.collectable[id] {
		return execution.StateRef{}, execution.ErrConflict
	}
	return source.refs[id], nil
}

func (source *retainedStateFixtureSource) ListProtectedStates(context.Context) ([]execution.StateRef, error) {
	var refs []execution.StateRef
	for _, ref := range source.refs {
		refs = append(refs, ref)
	}
	return refs, nil
}

func TestRetainedStateRoundTripIsolationAndCollection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	neighbor := source.add("10000000-0000-4000-8000-000000000002", "test-state")
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []execution.StateRef{ref, neighbor} {
		if err := states.Ensure(ctx, candidate); err != nil {
			t.Fatal(err)
		}
		source.initialized[candidate.ID] = true
		if err := os.WriteFile(filepath.Join(root, "assignment-"+candidate.ID, "runtime-state", "session.db"), []byte("session-"+candidate.ID), 0600); err != nil {
			t.Fatal(err)
		}
	}
	location := filepath.Join(root, "assignment-"+ref.ID, "runtime-state")
	if err := os.Mkdir(filepath.Join(location, "storage"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(location, "storage", "history.json"), []byte(`{"history":["prior turn"]}`), 0640); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive.Bytes(), []byte(neighbor.ID)) {
		t.Fatal("archive leaked another Participant's state")
	}
	if err := states.Collect(ctx, ref); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("protected collection = %v", err)
	}
	if err := os.RemoveAll(location); err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); !errors.Is(err, execution.ErrStateUnavailable) {
		t.Fatalf("missing initialized state = %v", err)
	}
	// Fresh adapter and separate compatible host path, same immutable namespace.
	restoredRoot := t.TempDir()
	restored, err := NewRetainedState(restoredRoot, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restored.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(filepath.Join(restoredRoot, "assignment-"+ref.ID, "runtime-state", "storage", "history.json"))
	if err != nil || string(content) != `{"history":["prior turn"]}` {
		t.Fatalf("restored history = %q, %v", content, err)
	}
	info, err := os.Stat(filepath.Join(restoredRoot, "assignment-"+ref.ID, "runtime-state", "storage"))
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("restored mode = %v, %v", info, err)
	}
	current := filepath.Join(restoredRoot, "assignment-"+ref.ID, "runtime-state", "session.db")
	if err := os.WriteFile(current, []byte("newer-session-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("restore overwrote different existing data: %v", err)
	}
	currentBytes, err := os.ReadFile(current)
	if err != nil || string(currentBytes) != "newer-session-data" {
		t.Fatalf("existing data changed: %q, %v", currentBytes, err)
	}
	source.collectable[ref.ID] = true
	for range 2 {
		if err := restored.Collect(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if err := states.Inspect(ctx, neighbor); err != nil {
		t.Fatalf("neighbor changed: %v", err)
	}
}

func TestRetainedStateRejectsInvalidArchivesWithoutPublishing(t *testing.T) {
	ctx := context.Background()
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	root := t.TempDir()
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	location := filepath.Join(root, "assignment-"+ref.ID, "runtime-state")
	if err := os.WriteFile(filepath.Join(location, "session.json"), []byte("original-history"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(location); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(archive.Bytes(), []byte("original-history"), []byte("modified-history"), 1)
	malicious := func(header *tar.Header) []byte {
		var content bytes.Buffer
		writer := tar.NewWriter(&content)
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return content.Bytes()
	}
	tests := map[string][]byte{
		"changed bytes":        corrupt,
		"partial content":      archive.Bytes()[:600],
		"missing terminator":   archive.Bytes()[:len(archive.Bytes())-1024],
		"hidden trailing data": append(bytes.Clone(archive.Bytes()), []byte("hidden")...),
		"parent traversal":     malicious(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0600}),
		"symlink":              malicious(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside", Mode: 0700}),
		"credentials":          malicious(&tar.Header{Name: "auth.json", Typeflag: tar.TypeReg, Mode: 0600}),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if err := states.Restore(ctx, ref, bytes.NewReader(content)); !errors.Is(err, execution.ErrCorruptState) {
				t.Fatalf("restore = %v", err)
			}
			if _, err := os.Lstat(location); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial restore published: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(location))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging was retained: %v, %v", entries, err)
			}
		})
	}
	other := ref
	other.SessionID = "20000000-0000-4000-8000-000000000001"
	if err := states.Restore(ctx, other, bytes.NewReader(archive.Bytes())); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("Session-level restore = %v", err)
	}
}

func TestRetainedStateRejectsBackendOwnershipAndUnsafeStorage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*execution.StateRef){
		func(ref *execution.StateRef) { ref.Backend.Family = "microsandbox" },
		func(ref *execution.StateRef) { ref.Backend.Version++ },
		func(ref *execution.StateRef) { ref.Backend.Namespace = "another-installation" },
		func(ref *execution.StateRef) { ref.Format = "pi/v1" },
		func(ref *execution.StateRef) { ref.ParticipantID = "10000000-0000-4000-8000-000000000002" },
	} {
		bad := ref
		change(&bad)
		if err := states.Ensure(ctx, bad); !errors.Is(err, execution.ErrBindingMismatch) {
			t.Fatalf("mismatched Ensure = %v", err)
		}
		if err := states.Export(ctx, bad, io.Discard); !errors.Is(err, execution.ErrBindingMismatch) {
			t.Fatalf("mismatched Export = %v", err)
		}
	}
	source.metadataVersion++
	if err := states.Ensure(ctx, ref); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("unsupported storage version = %v", err)
	}
	source.metadataVersion = 1
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "assignment-"+ref.ID)); err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("symlink storage = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("external storage changed: %v, %v", entries, err)
	}
}

func TestRetainedStateRestorePreservesPermissionsUnderPrivateUmask(t *testing.T) {
	// umask is process-global. Isolate it rather than affecting unrelated tests.
	if os.Getenv("OMNIGREX_TEST_STATE_UMASK") == "" {
		command := exec.Command(os.Args[0], "-test.run", "^TestRetainedStateRestorePreservesPermissionsUnderPrivateUmask$")
		command.Env = append(os.Environ(), "OMNIGREX_TEST_STATE_UMASK=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("private-umask restore: %v\n%s", err, output)
		}
		return
	}
	ctx := context.Background()
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	root := t.TempDir()
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "assignment-"+ref.ID, "runtime-state", "history.json")
	if err := os.WriteFile(file, []byte("retained history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0640); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	syscall.Umask(0077)
	for range 2 {
		if err := states.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("restored mode = %v, %v; want 0640", info, err)
	}
}

func TestRetainedStateRestoreRetryRemovesRestrictiveStaging(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// Repair fixture permissions even when the regression fails, so the test's
	// own temporary directory cleanup does not obscure the staging leak.
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return nil
		})
	})
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "assignment-"+ref.ID)
	readonly := filepath.Join(parent, "runtime-state", "readonly")
	if err := os.Mkdir(readonly, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "history.json"), []byte("retained history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0500); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := states.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 1 || entries[0].Name() != "runtime-state" {
			t.Fatalf("restore retry retained Session-data copies: %v, %v", entries, err)
		}
	}
	info, err := os.Stat(readonly)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("restore retry changed retained permissions: %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "history.json"), []byte("newer history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := states.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("conflicting restore = %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "runtime-state" {
		t.Fatalf("conflicting restore retained Session-data copies: %v, %v", entries, err)
	}
}

func TestRetainedStateCollectionRemovesInterruptedRestoreCopies(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return nil
		})
	})
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	neighbor := source.add("10000000-0000-4000-8000-000000000002", "test-state")
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []execution.StateRef{ref, neighbor} {
		if err := states.Ensure(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "assignment-"+ref.ID)
	neighborState := filepath.Join(root, "assignment-"+neighbor.ID, "runtime-state")
	for _, directory := range []string{
		filepath.Join(parent, "runtime-state", "readonly"),
		filepath.Join(parent, ".restore-"+strings.Repeat("a", 32), "readonly"),
		filepath.Join(root, "assignment-"+neighbor.ID, ".restore-"+strings.Repeat("b", 32), "readonly"),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "history.json"), []byte("retained history"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0000); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(neighborState, "history.json"), []byte("neighbor history"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(neighborState, 0500); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"other", ".restore-not-a-nonce"} {
		if err := os.Mkdir(filepath.Join(parent, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(neighborState, filepath.Join(parent, ".restore-"+strings.Repeat("c", 32))); err != nil {
		t.Fatal(err)
	}
	if err := states.Collect(ctx, ref); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("protected collection = %v", err)
	}
	source.collectable[ref.ID] = true
	for range 2 {
		if err := states.Collect(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 2 || entries[0].Name() != ".restore-not-a-nonce" || entries[1].Name() != "other" {
		t.Fatalf("collection retained Session-data copies or removed unrelated entries: %v, %v", entries, err)
	}
	info, err := os.Stat(neighborState)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("collection followed a staging link into neighboring state: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "assignment-"+neighbor.ID, ".restore-"+strings.Repeat("b", 32))); err != nil {
		t.Fatalf("collection removed neighboring restore copy: %v", err)
	}
}

type retainedStateEOFReader struct {
	io.Reader
	onEOF func()
}

func (reader retainedStateEOFReader) Read(buffer []byte) (int, error) {
	n, err := reader.Reader.Read(buffer)
	if err == io.EOF {
		reader.onEOF()
	}
	return n, err
}

func TestRetainedStateRestoreReportsStagingCleanupFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := newRetainedStateFixture()
	ref := source.add("10000000-0000-4000-8000-000000000001", "test-state")
	states, err := NewRetainedState(root, "test-state", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "assignment-"+ref.ID)
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	history := filepath.Join(parent, "runtime-state", "history.json")
	if err := os.WriteFile(history, []byte("retained history"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	input := retainedStateEOFReader{Reader: bytes.NewReader(archive.Bytes()), onEOF: func() {
		// A real filesystem failure prevents unlinking the disposable staging
		// root while still permitting the identical-data verification to finish.
		if err := os.Chmod(parent, 0500); err != nil {
			t.Fatal(err)
		}
	}}
	if err := states.Restore(ctx, ref, input); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("staging cleanup failure was hidden: %v", err)
	}
	content, err := os.ReadFile(history)
	if err != nil || string(content) != "retained history" {
		t.Fatalf("cleanup failure changed retained state: %q, %v", content, err)
	}
}
