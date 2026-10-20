package microsandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type deletingLogPlatform struct {
	*fakePlatform
	beforeRemove func(metadata)
	home         string
}

func (driver *deletingLogPlatform) Remove(ctx context.Context, meta metadata) error {
	driver.beforeRemove(meta)
	if err := os.RemoveAll(filepath.Join(driver.home, "sandboxes", meta.Name)); err != nil {
		return err
	}
	return driver.fakePlatform.Remove(ctx, meta)
}

func TestRuntimeLogsPreservedBeforeRemoval(t *testing.T) {
	for _, scenario := range []string{"normal", "reconstructed", "lost-ack", "duplicate", "orphan", "missing-main"} {
		t.Run(scenario, func(t *testing.T) {
			backend, driver, ledger := testBackend(t)
			request := testRequest()
			engine, err := backend.New(request, ledger)
			if err != nil {
				t.Fatal(err)
			}
			process, err := engine.Create(context.Background(), request, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			original := driver.resources[nameFor(request.Key)]
			resources := []metadata{original}
			if scenario == "duplicate" || scenario == "missing-main" {
				duplicate := original
				duplicate.Name += "-duplicate"
				duplicate.ID = "duplicate"
				driver.resources[duplicate.Name] = duplicate
				resources = append(resources, duplicate)
			}
			for _, meta := range resources {
				source := filepath.Join(backend.options.Home, "sandboxes", meta.Name, "logs")
				if err := os.MkdirAll(source, 0700); err != nil {
					t.Fatal(err)
				}
				writeArchiveFile(t, filepath.Join(source, "exec.log"), "native "+meta.ID)
			}
			backend.platform = &deletingLogPlatform{fakePlatform: driver, home: backend.options.Home, beforeRemove: func(meta metadata) {
				content, err := os.ReadFile(filepath.Join(archiveDestination(backend, request.Key, meta.ID), "exec.log"))
				if err != nil || string(content) != "native "+meta.ID {
					t.Errorf("archive unavailable before native deletion: %q, %v", content, err)
				}
			}}
			if scenario == "reconstructed" {
				backend = &Backend{options: backend.options, platform: backend.platform, ledger: ledger, inputs: newInputBindings()}
			}
			if scenario == "lost-ack" {
				driver.removeErr = errors.New("lost acknowledgement")
			}
			if scenario == "missing-main" {
				if err := backend.EnsureKeyStopped(context.Background(), request.Key); err != nil {
					t.Fatal(err)
				}
				delete(driver.resources, original.Name)
			}
			if scenario == "orphan" {
				err = backend.EnsureOrphansAbsent(context.Background(), request.Key, []execution.Ref{resourceRef(original)})
			} else if scenario == "normal" {
				err = process.Remove(context.Background())
			} else {
				err = backend.EnsureKeyAbsent(context.Background(), request.Key)
			}
			if scenario == "lost-ack" {
				if !errors.Is(err, driver.removeErr) {
					t.Fatal(err)
				}
				driver.removeErr = nil
				err = backend.EnsureKeyAbsent(context.Background(), request.Key)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, meta := range resources {
				if scenario == "missing-main" && meta.ID == original.ID {
					continue
				}
				content, err := os.ReadFile(filepath.Join(archiveDestination(backend, request.Key, meta.ID), "exec.log"))
				if err != nil || string(content) != "native "+meta.ID {
					t.Fatalf("retained=%q, %v", content, err)
				}
			}
		})
	}
}

func TestRuntimeLogArchiveFailureDoesNotBlockCleanup(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "destination error", true: "copy deadline"}[cancelled], func(t *testing.T) {
			output := captureDiagnostics(t)
			backend, meta, _, _ := archiveFixture(t)
			driver := backend.platform.(*fakePlatform)
			driver.removeErr = errors.New("native Remove failed")
			ctx := context.Background()
			if cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				writeArchiveFile(t, backend.options.StateRoot, "not a directory")
			}
			if err := backend.removeResource(ctx, meta); !errors.Is(err, driver.removeErr) {
				t.Fatal(err)
			}
			if driver.removes != 1 || !strings.Contains(output.String(), "WARN") || !strings.Contains(output.String(), "archive incomplete") {
				t.Fatalf("removes=%d, logs=%s", driver.removes, output)
			}
		})
	}
}
