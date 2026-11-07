package workspace

import (
	"context"
	"errors"
	"os"

	"github.com/jozala/omnigrex/internal/telemetry"
	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

type mountedRoot struct{ resource, parent, leaf string }

func (monitor *ToolCacheMonitor) mountedRoots() []mountedRoot {
	return []mountedRoot{
		{"workspace", monitor.lifecycle.workspaceRoot, "workspace"},
		{"state", monitor.stateRoot, "runtime-state"},
		{"mise", monitor.lifecycle.miseRoot, "mise"},
		{"tool-data", monitor.lifecycle.miseRoot, "tool-data"},
	}
}

// Open only configured orchestrator-mounted backing paths. Both supplied backend
// mappings use these same Participant subdirectories; never consult inventories
// or daemon host paths. No permission repair or cleanup helper is used.
func openMountedRoot(root mountedRoot, id string) (*os.File, unix.Stat_t, error) {
	if !uuidtext.Valid(id) {
		return nil, unix.Stat_t{}, ErrInvalidAssignmentID
	}
	parent, _, err := openDirectoryNoFollow(root.parent)
	var stat unix.Stat_t
	if err == nil {
		err = unix.Fstat(int(parent.Fd()), &stat)
		if err != nil {
			parent.Close()
		}
	}
	if err != nil {
		return nil, stat, err
	}
	if int(stat.Uid) != os.Geteuid() {
		parent.Close()
		return nil, stat, ErrUnsafeAssignmentPath
	}
	dev := uint64(stat.Dev)
	for _, name := range []string{"assignment-" + id, root.leaf} {
		child, opened, err := openCacheDirectoryAt(parent, name, dev)
		parent.Close()
		if err != nil {
			return nil, opened, err
		}
		parent, stat = child, opened
	}
	return parent, stat, nil
}

// observeMounted shares the existing bounded, resumable no-follow traversal.
// Its state is independent of cache warnings and never changes their watermark.
func (monitor *ToolCacheMonitor) observeMounted(ctx context.Context, id string, devices map[uint64]bool) {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	for _, root := range monitor.mountedRoots() {
		if root.parent == "" {
			continue
		} // Historical/test callers may omit state.
		key := id + "/" + root.resource
		directory, stat, err := openMountedRoot(root, id)
		status := "unavailable"
		if os.IsNotExist(err) {
			status = "missing"
		}
		if err != nil {
			if scan := monitor.mountedScans[key]; scan != nil {
				scan.Close()
				delete(monitor.mountedScans, key)
			}
			telemetry.RecordStorage(ctx, root.resource, status, nil)
			continue
		}
		dev := uint64(stat.Dev)
		if !devices[dev] {
			var fs unix.Statfs_t
			if unix.Fstatfs(int(directory.Fd()), &fs) == nil {
				devices[dev] = true
				capacity, free, available := float64(fs.Blocks)*float64(fs.Bsize), float64(fs.Bfree)*float64(fs.Bsize), float64(fs.Bavail)*float64(fs.Bsize)
				inodes, freeInodes := float64(fs.Files), float64(fs.Ffree)
				if fs.Files > 0 {
					telemetry.RecordFilesystem(ctx, &capacity, &free, &available, &inodes, &freeInodes)
				} else {
					// Filesystems without inode capacity reporting must not look empty.
					telemetry.RecordFilesystem(ctx, &capacity, &free, &available, nil, nil)
				}
			}
		}
		scan := monitor.mountedScans[key]
		if scan != nil && scan.root != cacheIdentity(stat) {
			scan.Close()
			delete(monitor.mountedScans, key)
			scan = nil
		}
		if scan == nil {
			if len(monitor.mountedScans) >= maxActiveToolCacheScans {
				directory.Close()
				telemetry.RecordStorage(ctx, root.resource, "unavailable", nil)
				continue
			}
			scan = newToolCacheScan(directory, stat)
			monitor.mountedScans[key] = scan
		} else {
			directory.Close()
		}
		usage, err := scan.Step(ctx, monitor.entryBudget)
		status = "complete"
		if usage.Truncated {
			status = "partial"
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			telemetry.RecordStorage(ctx, root.resource, "unavailable", nil)
			scan.Close()
			delete(monitor.mountedScans, key)
			continue
		}
		telemetry.RecordStorage(ctx, root.resource, status, &usage.Bytes)
		if len(scan.frames) == 0 {
			scan.Close()
			delete(monitor.mountedScans, key)
		}
	}
}
