package microsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"golang.org/x/sys/unix"
)

func archiveFixture(t *testing.T) (*Backend, metadata, string, string) {
	t.Helper()
	backend, driver, _ := testBackend(t)
	key := testRequest().Key
	identity, _ := json.Marshal(key)
	meta := metadata{ID: "original", Name: nameFor(key), Status: "Stopped", Labels: map[string]string{namespaceLabel: "tests", identityLabel: string(identity), digestLabel: strings.Repeat("a", 64)}}
	driver.resources[meta.Name] = meta
	source := filepath.Join(backend.options.Home, "sandboxes", meta.Name, "logs")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	return backend, meta, source, archiveDestination(backend, key, meta.ID)
}

func archiveDestination(backend *Backend, key execution.Key, id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(backend.options.StateRoot, "assignment-"+key.AssignmentID, "runtime-logs", key.AgentTurnID+"-1", hex.EncodeToString(digest[:]))
}

func writeArchiveFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLogsArchivePreservesAllFiles(t *testing.T) {
	backend, meta, source, destination := archiveFixture(t)
	files := map[string]string{"runtime.log": "runtime\x00raw", "kernel.log": "kernel", "exec.log": "exec", "exec.log.1": "rotated", "kernel.log.2": "old kernel"}
	for name, content := range files {
		writeArchiveFile(t, filepath.Join(source, name), content)
	}
	writeArchiveFile(t, filepath.Join(source, "secret"), "unrelated")
	writeArchiveFile(t, filepath.Join(source, "exec.log.01"), "not canonical")
	if err := backend.archiveRuntimeLogs(context.Background(), testRequest().Key, meta); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		content, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(content) != want {
			t.Fatalf("%s = %q, %v", name, content, err)
		}
		info, _ := os.Stat(filepath.Join(destination, name))
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	entries, _ := os.ReadDir(destination)
	if len(entries) != len(files)+1 {
		t.Fatal(entries)
	}
	var manifest struct {
		Key      execution.Key
		Resource metadata
		Files    map[string]string
	}
	content, err := os.ReadFile(filepath.Join(destination, "archive.json"))
	if err != nil || json.Unmarshal(content, &manifest) != nil || manifest.Key != testRequest().Key || manifest.Resource.ID != meta.ID || manifest.Files["exec.log"] != "copied" {
		t.Fatalf("manifest=%s, %v", content, err)
	}
	info, _ := os.Stat(destination)
	if info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}
	meta.ID = "duplicate"
	backend.platform.(*fakePlatform).resources[meta.Name] = meta
	if err := backend.archiveRuntimeLogs(context.Background(), testRequest().Key, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(archiveDestination(backend, testRequest().Key, meta.ID), "exec.log")); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLogsArchiveRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"source-link", "parent-link", "hard-link", "fifo", "destination-link", "foreign", "diagnostic", "traversal", "replacement", "invalid-key"} {
		t.Run(kind, func(t *testing.T) {
			backend, meta, source, destination := archiveFixture(t)
			outside := filepath.Join(t.TempDir(), "outside")
			writeArchiveFile(t, outside, "private")
			key := testRequest().Key
			switch kind {
			case "source-link":
				if err := os.Symlink(outside, filepath.Join(source, "exec.log")); err != nil {
					t.Fatal(err)
				}
			case "hard-link":
				if err := os.Link(outside, filepath.Join(source, "exec.log")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(filepath.Join(source, "exec.log"), 0600); err != nil {
					t.Fatal(err)
				}
			case "parent-link":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), source); err != nil {
					t.Fatal(err)
				}
			case "destination-link":
				if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), destination); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				meta.Labels[namespaceLabel] = "other"
			case "diagnostic":
				meta.Labels[diagnosticLabel] = "true"
			case "traversal":
				meta.Name = "../escape"
			case "replacement":
				replacement := meta
				replacement.ID = "replacement"
				backend.platform.(*fakePlatform).resources[meta.Name] = replacement
			case "invalid-key":
				key.AssignmentID = "../escape"
			}
			if err := backend.archiveRuntimeLogs(context.Background(), key, meta); err == nil {
				t.Fatal("unsafe archive succeeded")
			}
			if content, _ := os.ReadFile(outside); string(content) != "private" {
				t.Fatal("outside file changed")
			}
			_ = filepath.WalkDir(destination, func(path string, entry os.DirEntry, err error) error {
				if err == nil && entry.Type().IsRegular() {
					content, _ := os.ReadFile(path)
					if strings.Contains(string(content), "private") {
						t.Fatal("private data archived")
					}
				}
				return nil
			})
		})
	}
}

func TestRuntimeLogsArchiveRetryAndPartialFailure(t *testing.T) {
	backend, meta, source, destination := archiveFixture(t)
	writeArchiveFile(t, filepath.Join(source, "exec.log"), "complete")
	if err := backend.archiveRuntimeLogs(context.Background(), testRequest().Key, meta); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(destination, "kernel.log"), 0700); err != nil {
		t.Fatal(err)
	}
	writeArchiveFile(t, filepath.Join(source, "kernel.log"), "new")
	if err := backend.archiveRuntimeLogs(context.Background(), testRequest().Key, meta); err == nil {
		t.Fatal("structural conflict ignored")
	}
	if content, _ := os.ReadFile(filepath.Join(destination, "exec.log")); string(content) != "complete" {
		t.Fatal("previous log lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := backend.archiveRuntimeLogs(ctx, testRequest().Key, meta); err == nil {
		t.Fatal("cancellation ignored")
	}
	entries, _ := os.ReadDir(filepath.Dir(destination))
	if len(entries) != 1 {
		t.Fatal(entries)
	}
	entries, _ = os.ReadDir(destination)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".archive-") {
			t.Fatal("partial staging leaked")
		}
	}
	var manifest struct{ Files map[string]string }
	content, _ := os.ReadFile(filepath.Join(destination, "archive.json"))
	if json.Unmarshal(content, &manifest) != nil || manifest.Files["runtime.log"] != "missing" {
		t.Fatalf("missing files not recorded: %s", content)
	}
}
