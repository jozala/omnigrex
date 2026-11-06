package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMountedRootsAreReadOnlyAndNoFollow(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	parent := t.TempDir()
	if _, _, err := openMountedRoot(mountedRoot{"workspace", parent, "workspace"}, "../unsafe"); err != ErrInvalidAssignmentID {
		t.Fatalf("unsafe ID: %v", err)
	}
	participant := filepath.Join(parent, "assignment-"+id)
	if err := os.Mkdir(participant, 0700); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"workspace", "runtime-state", "mise", "tool-data"} {
		t.Run(resource, func(t *testing.T) {
			root := mountedRoot{resource, parent, resource}
			path := filepath.Join(participant, resource)
			if _, _, err := openMountedRoot(root, id); !os.IsNotExist(err) {
				t.Fatalf("missing=%v", err)
			}
			outside := t.TempDir()
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if file, _, err := openMountedRoot(root, id); err == nil {
				file.Close()
				t.Fatal("followed symlink")
			}
			os.Remove(path)
			os.Mkdir(path, 0700)
			file, stat, err := openMountedRoot(root, id)
			if err != nil {
				t.Fatal(err)
			}
			scan := newToolCacheScan(file, stat)
			usage, err := scan.Step(context.Background(), 100)
			scan.Close()
			if err != nil || usage.Truncated || usage.Files != 0 {
				t.Fatalf("empty=%+v %v", usage, err)
			}
			if err := os.WriteFile(filepath.Join(path, "sentinel-secret"), make([]byte, 8192), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
				t.Fatal(err)
			}
			file, stat, err = openMountedRoot(root, id)
			if err != nil {
				t.Fatal(err)
			}
			scan = newToolCacheScan(file, stat)
			usage, err = scan.Step(context.Background(), 0)
			if err != nil || !usage.Truncated {
				t.Fatalf("partial=%+v %v", usage, err)
			}
			usage, err = scan.Step(context.Background(), 100)
			scan.Close()
			if err != nil || usage.Truncated || usage.Files != 1 || usage.Bytes < 8192 {
				t.Fatalf("allocated=%+v %v", usage, err)
			}
			os.Chmod(path, 0)
			if file, _, err := openMountedRoot(root, id); err == nil {
				file.Close()
				t.Fatal("inaccessible root opened")
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0 {
				t.Fatal("measurement repaired permissions")
			}
			os.Chmod(path, 0700)
			old := path + "-old"
			os.Rename(path, old)
			os.Mkdir(path, 0700)
			file, replacement, err := openMountedRoot(root, id)
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			if cacheIdentity(stat) == cacheIdentity(replacement) {
				t.Fatal("replacement identity unchanged")
			}
		})
	}
}

func TestMountedDiscoveryIncludesIdleStateOnlyParticipant(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "assignment-"+id, "runtime-state"), 0700); err != nil {
		t.Fatal(err)
	}
	monitor := &ToolCacheMonitor{lifecycle: &Lifecycle{workspaceRoot: t.TempDir(), miseRoot: t.TempDir()}, stateRoot: state, mountedScans: make(map[string]*toolCacheScan), entryBudget: 1}
	defer monitor.Close()
	ids, err := monitor.listMountedParticipantIDs(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("idle discovery=%v %v", ids, err)
	}
	devices := make(map[uint64]bool)
	monitor.observeMounted(context.Background(), id, devices)
	if len(devices) != 1 {
		t.Fatalf("shared filesystem observations=%d", len(devices))
	}
	monitor.observeMounted(context.Background(), id, devices)
	if len(devices) != 1 {
		t.Fatal("duplicated shared filesystem")
	}
	var stat unix.Stat_t
	if err := unix.Stat(state, &stat); err != nil || !devices[uint64(stat.Dev)] {
		t.Fatalf("wrong device: %v", err)
	}
}

func TestMountedReplacementAndDisappearanceDropRetainedScan(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	mise := t.TempDir()
	root := filepath.Join(mise, "assignment-"+id, "tool-data")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private-file"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	monitor := &ToolCacheMonitor{lifecycle: &Lifecycle{workspaceRoot: t.TempDir(), miseRoot: mise}, mountedScans: make(map[string]*toolCacheScan), entryBudget: 0}
	defer monitor.Close()
	monitor.observeMounted(context.Background(), id, make(map[uint64]bool))
	key := id + "/tool-data"
	original := monitor.mountedScans[key]
	if original == nil {
		t.Fatal("missing resumable scan")
	}
	os.Rename(root, root+"-old")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "new"), []byte("x"), 0600)
	monitor.observeMounted(context.Background(), id, make(map[uint64]bool))
	if monitor.mountedScans[key] == original || len(original.frames) != 0 {
		t.Fatal("replacement retained obsolete scan/handles")
	}
	replacement := monitor.mountedScans[key]
	os.RemoveAll(root)
	monitor.observeMounted(context.Background(), id, make(map[uint64]bool))
	if monitor.mountedScans[key] != nil || len(replacement.frames) != 0 {
		t.Fatal("disappearance retained scan/handles")
	}
}

func TestMountedDiscoveryFailureDoesNotSuppressCacheWarning(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	mise := t.TempDir()
	root := filepath.Join(mise, "assignment-"+id, "tool-data", "assignment")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cache"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	inaccessible := t.TempDir()
	os.Chmod(inaccessible, 0)
	defer os.Chmod(inaccessible, 0700)
	monitor, err := NewToolCacheMonitor(&Lifecycle{workspaceRoot: inaccessible, miseRoot: mise}, ToolCacheMonitorConfig{ThresholdBytes: 1, PollInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	checked, warned, err := monitor.Check(context.Background())
	if err != nil || checked != 1 || warned != 1 {
		t.Fatalf("cache warning regressed: checked=%d warned=%d err=%v", checked, warned, err)
	}
	_, warned, err = monitor.Check(context.Background())
	if err != nil || warned != 0 {
		t.Fatalf("cache warning suppression regressed: warned=%d err=%v", warned, err)
	}
}
