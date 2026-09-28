package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestTurnPathsKeepAssignmentCacheAndRemoveOnlyTurnScratch(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	turn := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	configuration := turnconfig.Configuration{Directories: map[string]turnconfig.Lifecycle{"build": turnconfig.Turn, "cache": turnconfig.Assignment}, Environment: map[string]string{"TMPDIR": "build", "GOCACHE": "cache"}}
	values, err := lifecycle.PrepareTurnPaths(context.Background(), assignment, turn, configuration, nil)
	if err != nil || values["TMPDIR"] != "/home/opencode/.local/share/omnigrex-tool-data/turn/"+turn+"/build" || values["GOCACHE"] != "/home/opencode/.local/share/omnigrex-tool-data/assignment/cache" {
		t.Fatalf("paths = %#v, %v", values, err)
	}
	cache := filepath.Join(root, "mise", "assignment-"+assignment, "tool-data", "assignment", "cache")
	if err := os.WriteFile(filepath.Join(cache, "reuse"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.CleanupTurnPaths(context.Background(), assignment, turn, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "reuse")); err != nil {
		t.Fatalf("cache lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mise", "assignment-"+assignment, "tool-data", "turn", turn)); !os.IsNotExist(err) {
		t.Fatalf("scratch retained: %v", err)
	}
	if err := lifecycle.CleanupAssignmentToolPaths(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("collected Assignment retained cache: %v", err)
	}
}

func TestTurnPathCollectionRefusesSymlinkedAssignmentData(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	turn := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	configuration := turnconfig.Configuration{Directories: map[string]turnconfig.Lifecycle{"cache": turnconfig.Assignment}, Environment: map[string]string{"GOCACHE": "cache"}}
	if _, err := lifecycle.PrepareTurnPaths(context.Background(), assignment, turn, configuration, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "mise", "assignment-"+assignment, "tool-data")
	outside := filepath.Join(root, "unrelated")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "retain"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-detached"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.CleanupAssignmentToolPaths(context.Background(), assignment); err == nil {
		t.Fatal("followed symlinked tool data")
	}
	if _, err := os.Stat(filepath.Join(outside, "retain")); err != nil {
		t.Fatalf("outside data changed: %v", err)
	}
}

func TestTurnPathCleanupRestoresUnreadableOwnedParentsWithoutTouchingCache(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	turn := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	configuration := turnconfig.Configuration{
		Directories: map[string]turnconfig.Lifecycle{"build": turnconfig.Turn, "cache": turnconfig.Assignment},
		Environment: map[string]string{"TMPDIR": "build", "GOCACHE": "cache"},
	}
	if _, err := lifecycle.PrepareTurnPaths(context.Background(), assignment, turn, configuration, nil); err != nil {
		t.Fatal(err)
	}
	toolData := filepath.Join(root, "mise", "assignment-"+assignment, "tool-data")
	cache := filepath.Join(toolData, "assignment", "cache", "retained")
	if err := os.WriteFile(cache, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	turnParent := filepath.Join(toolData, "turn")
	if err := os.Chmod(turnParent, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(toolData, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(toolData, 0o700); _ = os.Chmod(turnParent, 0o700) })
	if err := lifecycle.CleanupTurnPaths(context.Background(), assignment, turn, nil); err != nil {
		t.Fatalf("clean scratch with unreadable parents: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache was changed by scratch cleanup: %v", err)
	}
	if err := os.Chmod(turnParent, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(toolData, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.PrepareTurnPaths(context.Background(), assignment, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", configuration, nil); err != nil {
		t.Fatalf("prepare next turn after unreadable parent: %v", err)
	}
}
