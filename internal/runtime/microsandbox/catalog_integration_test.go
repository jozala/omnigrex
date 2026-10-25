//go:build integration && microsandbox && cgo && linux

package microsandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

// Keep the production SDK in this process alive across VM lifetimes and many
// independent catalog opens/closes. In particular, never open the DB directly
// here: even a diagnostic open/close can release SQLite's process-wide locks.
func TestNativeCatalogLongevity(t *testing.T) {
	backend, request, parentCtx := nativeFixture(t)
	ctx, cancel := context.WithTimeout(parentCtx, 8*time.Minute)
	defer cancel()
	const iterations = 16
	started := time.Now()
	prefix := fmt.Sprintf("catalog-%d", started.UnixNano())
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range names {
			if err := sdk.RemoveVolume(cleanup, name); err != nil {
				t.Error(err)
			}
		}
	})
	for i := 0; i < iterations; i++ {
		parent := fmt.Sprintf("%s-parent-%d", prefix, i)
		if _, err := sdk.CreateVolume(ctx, parent); err != nil {
			t.Fatal(err)
		}
		names = append(names, parent)
		peer := fmt.Sprintf("%s-peer-%d", prefix, i)
		catalogPeer(t, ctx, backend.options.Home, "create", peer, strings.Join(names, ","))
		names = append(names, peer)
		assertCatalogVolumes(t, ctx, names)
		assertNoDeletedCatalogMappings(t)

		// A fresh execution identity per iteration also exercises repeated Reviewer
		// admission after the previous Turn's capture/flush/cleanup has settled.
		current := request
		current.Key.ExecutionEpoch = int64(i + 1)
		current.Command = []string{"/bin/sh", "-ec", "printf catalog-complete"}
		workspace := backend.location(current.Key, "workspace")
		if err := os.MkdirAll(workspace, 0700); err != nil {
			t.Fatal(err)
		}
		current.Workspace = execution.WorkspaceRef{Key: current.Key, Seed: strings.Repeat("a", 40)}
		current.Attachments = []execution.Attachment{{Resource: "workspace", Target: "/workspace"}}
		engine, err := backend.New(current, backend.ledger)
		if err != nil {
			t.Fatal(err)
		}
		process, err := engine.Create(ctx, current, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Transport().Close(); _ = engine.EnsureAbsent(context.Background()) })
		if err := process.Start(ctx); err != nil {
			t.Fatal(err)
		}
		type completion struct {
			output []byte
			err    error
		}
		completed := make(chan completion, 1)
		go func() { output, err := io.ReadAll(process.Transport()); completed <- completion{output, err} }()
		select {
		case result := <-completed:
			if result.err != nil || string(result.output) != "catalog-complete" {
				t.Fatalf("normal completion: %q, %v", result.output, result.err)
			}
		case <-ctx.Done():
			_ = process.Transport().Close()
			t.Fatal("catalog lifecycle exceeded eight-minute bound")
		}
		if exit, err := process.ObserveExit(ctx); err != nil || !exit.Exited || !exit.ExitCodeKnown || exit.ExitCode != 0 {
			t.Fatalf("exit: %+v, %v", exit, err)
		}
		if err := backend.ReadWorkspace(ctx, execution.CaptureRequest{Workspace: current.Workspace, OperationID: fmt.Sprintf("catalog-publication-%d", i), Purpose: execution.PublicationCapture}, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }); err != nil {
			t.Fatal(err)
		}
		if err := process.Stop(ctx, 20*time.Second); err != nil {
			t.Fatal(err)
		}
		// Recreate the backend after required flush/stop, losing ephemeral input
		// bindings but retaining the journal and ledger. Cleanup is identity-fenced.
		restarted, err := New(backend.options, backend.ledger)
		if err != nil {
			t.Fatal(err)
		}
		if err := restarted.EnsureKeyAbsent(ctx, current.Key); err != nil {
			t.Fatal(err)
		}
		if err := restarted.EnsureKeyAbsent(ctx, current.Key); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Create(ctx, current, io.Discard); err == nil {
			t.Fatal("revoked execution was replayed")
		}
		if err := restarted.Close(); err != nil {
			t.Fatal(err)
		}
		if err := process.Transport().Close(); err != nil {
			t.Fatal(err)
		}
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}

		catalogPeer(t, ctx, backend.options.Home, "verify", "", strings.Join(names, ","))
		assertCatalogVolumes(t, ctx, names)
		catalogIntegrity(t, ctx, backend.options.Home)
		assertNoDeletedCatalogMappings(t)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertCatalogVolumes(t, ctx, names)
	}
	t.Logf("CATALOG QUALIFICATION PASS: iterations=%d elapsed=%s long_lived_pid=%d independent_sdk_processes=%d idle_gaps=16x3s acknowledged_volumes=%d integrity=ok deleted_wal_shm=none capture_flush_stop_recovery=pass", iterations, time.Since(started).Round(time.Millisecond), os.Getpid(), iterations*2, len(names))
}

func catalogPeer(t *testing.T, ctx context.Context, home, mode, name, expected string) {
	t.Helper()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeCatalogPeer$", "-test.v")
	command.Env = append(os.Environ(), "OMNIGREX_CATALOG_PEER="+mode, "MSB_HOME="+home, "OMNIGREX_CATALOG_NAME="+name, "OMNIGREX_CATALOG_EXPECTED="+expected)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("catalog SDK peer: %s, %v", output, err)
	}
	if !strings.Contains(string(output), "CATALOG PEER PASS") {
		t.Fatal("peer did not execute catalog assertions")
	}
}

func TestNativeCatalogPeer(t *testing.T) {
	mode := os.Getenv("OMNIGREX_CATALOG_PEER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if mode == "create" {
		if _, err := sdk.CreateVolume(ctx, os.Getenv("OMNIGREX_CATALOG_NAME")); err != nil {
			t.Fatal(err)
		}
	} else if mode != "verify" && mode != "verify-retained" {
		t.Fatal("invalid peer mode")
	}
	assertCatalogVolumes(t, ctx, strings.Split(os.Getenv("OMNIGREX_CATALOG_EXPECTED"), ","))
	if mode == "verify-retained" {
		verifyRetainedCatalogData(t, ctx)
	}
	assertNoDeletedCatalogMappings(t)
	t.Logf("CATALOG PEER PASS pid=%d mode=%s", os.Getpid(), mode)
}

func assertCatalogVolumes(t *testing.T, ctx context.Context, names []string) {
	t.Helper()
	volumes, err := sdk.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, volume := range volumes {
		found[volume.Name()] = true
	}
	for _, name := range names {
		if !found[name] {
			t.Fatalf("acknowledged volume absent from inventory: %s", name)
		}
		volume, err := sdk.GetVolume(ctx, name)
		if err != nil || volume.Name() != name {
			t.Fatalf("acknowledged volume lookup %s: %v", name, err)
		}
	}
}

func assertNoDeletedCatalogMappings(t *testing.T) {
	t.Helper()
	mappings, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mappings), "\n") {
		if strings.Contains(line, "(deleted)") && (strings.Contains(line, "-shm") || strings.Contains(line, "-wal")) {
			t.Fatal("stale deleted SQLite mapping")
		}
	}
}

func catalogIntegrity(t *testing.T, ctx context.Context, home string) {
	t.Helper()
	// Metadata-only traversal in this process; sqlite3 opens the live catalog in
	// an independent process, not an offline copy and not the SDK's lock owner.
	count := 0
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "lib" || entry.Name() == "cache" || entry.Name() == "sandboxes") {
			return filepath.SkipDir
		}
		if entry.IsDir() || (!strings.HasSuffix(path, ".db") && !strings.HasSuffix(path, ".sqlite") && !strings.HasSuffix(path, ".sqlite3")) {
			return nil
		}
		command := exec.CommandContext(ctx, "sqlite3", "-readonly", path, "PRAGMA integrity_check;")
		output, err := command.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != "ok" {
			return fmt.Errorf("live catalog integrity: %q, %w", output, err)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no live catalog database checked")
	}
}
