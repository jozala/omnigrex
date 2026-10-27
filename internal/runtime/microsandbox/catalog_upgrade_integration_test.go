//go:build integration && microsandbox && cgo && linux

package microsandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

// Exercise both migrations on a disposable, quiescent copy, then recover using
// the pre-upgrade copy. No production files or migration records are edited.
func TestNativeCatalogUpgradeAndBackupRecovery(t *testing.T) {
	if os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("requires native qualification image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "cat-up-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	source := filepath.Join(root, "home")
	backup := filepath.Join(root, "backup")
	old := func(home, mode string, wantSuccess bool) {
		t.Helper()
		command := exec.CommandContext(ctx, "/usr/local/bin/catalog-v076", mode)
		command.Env = append(os.Environ(), "MSB_HOME="+home)
		output, err := command.CombinedOutput()
		if wantSuccess {
			if err != nil || !strings.Contains(string(output), "V076 CATALOG PASS") {
				t.Fatalf("v0.7.6 fixture: %s, %v", output, err)
			}
		} else if err == nil || !strings.Contains(strings.ToLower(string(output)), "migration") {
			t.Fatalf("old reader must reject upgraded migration history: %s, %v", output, err)
		}
	}
	copyHome := func(from, to string) {
		t.Helper()
		// Processes have exited before copying: DB, WAL and SHM are one quiescent
		// recovery set, not an online naked database-file copy.
		if output, err := exec.CommandContext(ctx, "cp", "-a", from, to).CombinedOutput(); err != nil {
			t.Fatalf("copy catalog home: %s, %v", output, err)
		}
	}
	old(source, "seed", true)
	// Preserve the native absolute storage namespace, as on operator restore.
	if err := os.Rename(source, backup); err != nil {
		t.Fatal(err)
	}
	copyHome(backup, source)
	catalogPeer(t, ctx, source, "verify-retained", "", "retained-v076")
	catalogPeer(t, ctx, source, "verify-retained", "", "retained-v076")
	catalogIntegrity(t, ctx, source)
	old(source, "verify", false)
	if err := os.Rename(source, filepath.Join(root, "upgraded-fenced")); err != nil {
		t.Fatal(err)
	}
	copyHome(backup, source)
	old(source, "verify", true)
	catalogIntegrity(t, ctx, source)
	t.Log("CATALOG UPGRADE PASS: source=0.7.6 candidate=0.7.11 independent_sdk_restarts=2 forward_migration=pass retained_data=pass old_reader=rejected backup_recovery=pass integrity=ok")
}

func verifyRetainedCatalogData(t *testing.T, ctx context.Context) {
	t.Helper()
	volume, err := sdk.GetVolume(ctx, "retained-v076")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(volume.Path(), "retained.txt"))
	if err != nil || string(data) != "acknowledged-v076" {
		t.Fatalf("migrated retained content: %q, %v", data, err)
	}
}
