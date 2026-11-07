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

func TestMountedDiscoveryKeepsHealthyRootsAfterFailure(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	for _, failed := range []string{"state", "mise", "workspace"} {
		for _, failure := range []string{"not-directory", "inaccessible", "symlink"} {
			t.Run(failed+"/"+failure, func(t *testing.T) {
				roots := map[string]string{"workspace": t.TempDir(), "mise": t.TempDir(), "state": t.TempDir()}
				healthy, leaf := "workspace", "workspace"
				if failed == "workspace" {
					healthy, leaf = "state", "runtime-state"
				}
				tree := filepath.Join(roots[healthy], "assignment-"+id, leaf)
				if err := os.MkdirAll(tree, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(tree, "data"), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "not-directory":
					path := filepath.Join(t.TempDir(), "root")
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
					roots[failed] = path
				case "inaccessible":
					if err := os.Chmod(roots[failed], 0); err != nil {
						t.Fatal(err)
					}
					defer os.Chmod(roots[failed], 0700)
				case "symlink":
					path := filepath.Join(t.TempDir(), "root")
					if err := os.Symlink(roots[failed], path); err != nil {
						t.Fatal(err)
					}
					roots[failed] = path
				}
				monitor, err := NewToolCacheMonitor(&Lifecycle{workspaceRoot: roots["workspace"], miseRoot: roots["mise"]}, ToolCacheMonitorConfig{StateRoot: roots["state"], ThresholdBytes: 1, PollInterval: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				defer monitor.Close()
				monitor.entryBudget = 0
				ids, err := monitor.listMountedParticipantIDs(context.Background())
				if err == nil || len(ids) != 1 || ids[0] != id {
					t.Fatalf("partial discovery=%v, %v; want healthy Participant and incomplete status", ids, err)
				}
				checked, _, err := monitor.Check(context.Background())
				if err != nil || checked != 1 {
					t.Fatalf("healthy-root check=%d, %v", checked, err)
				}
				if scan := monitor.mountedScans[id+"/"+healthy]; scan == nil || len(scan.frames) == 0 {
					t.Fatal("healthy idle tree was not measured")
				}
			})
		}
	}
}

func TestIncompleteMountedDiscoveryPreservesRetainedMonitoringState(t *testing.T) {
	for _, failure := range []string{"not-directory", "child-lookup-denied"} {
		t.Run(failure, func(t *testing.T) {
			const retained = "10000000-0000-4000-8000-000000000001"
			const healthy = "10000000-0000-4000-8000-000000000002"
			mise, workspace := t.TempDir(), t.TempDir()
			cache := filepath.Join(mise, "assignment-"+retained, "tool-data", "assignment")
			if err := os.MkdirAll(cache, 0700); err != nil {
				t.Fatal(err)
			}
			// Either first entry is an allocated file. Visit one, leaving the other
			// unread so the warning does not depend on directory st_blocks (e.g. APFS).
			for _, name := range []string{"data-a", "data-b"} {
				if err := os.WriteFile(filepath.Join(cache, name), make([]byte, 8192), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tree := filepath.Join(workspace, "assignment-"+healthy, "workspace")
			if err := os.MkdirAll(tree, 0700); err != nil {
				t.Fatal(err)
			}
			monitor, err := NewToolCacheMonitor(&Lifecycle{workspaceRoot: workspace, miseRoot: mise}, ToolCacheMonitorConfig{ThresholdBytes: 1, PollInterval: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer monitor.Close()
			monitor.entryBudget = 1
			usage, warned, err := monitor.Observe(context.Background(), retained)
			if err != nil || !warned || usage.Files != 1 || !usage.Truncated || usage.Bytes < 8192 {
				t.Fatalf("warning fixture=%+v warned=%v err=%v", usage, warned, err)
			}
			monitor.observeMounted(context.Background(), retained, make(map[uint64]bool))
			scan, mounted, watermark := monitor.scans[retained], monitor.mountedScans[retained+"/tool-data"], monitor.lastWarned[retained]
			if scan == nil || mounted == nil {
				t.Fatal("fixture did not retain scans")
			}
			if failure == "not-directory" {
				if err := os.Rename(mise, mise+"-old"); err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(mise + "-old")
				if err := os.WriteFile(mise, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(mise, 0400); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(mise, 0700)
				ids, err := listParticipantDirectories(context.Background(), mise)
				if err == nil || len(ids) != 0 {
					t.Fatalf("non-searchable directory discovery=%v err=%v; want incomplete", ids, err)
				}
			}
			checked, _, err := monitor.Check(context.Background())
			if err != nil || checked != 1 {
				t.Fatalf("healthy observation=%d %v", checked, err)
			}
			if monitor.scans[retained] != scan || monitor.mountedScans[retained+"/tool-data"] != mounted || monitor.lastWarned[retained] != watermark || len(scan.frames) == 0 || len(mounted.frames) == 0 {
				t.Fatal("incomplete discovery pruned retained scan/warning state")
			}
			// Complete discovery can subsequently establish that the old owner is gone.
			if failure == "not-directory" {
				if err := os.Remove(mise); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(mise, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(mise, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(mise, "assignment-"+retained)); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := monitor.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			if monitor.scans[retained] != nil || monitor.mountedScans[retained+"/tool-data"] != nil || len(scan.frames) != 0 || len(mounted.frames) != 0 {
				t.Fatal("complete discovery did not release stale scans")
			}
			if _, present := monitor.lastWarned[retained]; present {
				t.Fatal("complete discovery retained stale warning")
			}
		})
	}
}
