package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

// ToolCacheWarningThresholdReason documents the default soft signal.
// Assignment tool-data caches on the disk-backed mise volume survive turns
// and are collected only with their Assignment. A 1 GiB warning surfaces
// unusual growth without imposing a hard quota.
const (
	maxToolCacheEntries            = 500_000
	maxToolCacheAssignmentsPerPoll = 2000
	toolCachePerAssignmentTimeout  = 30 * time.Second
)

// ToolCacheUsage is the bounded operational measurement for one Assignment's
// assignment-lifecycle cache. Turn-lifecycle scratch is never included.
type ToolCacheUsage struct {
	Bytes     int64
	Files     int64
	Truncated bool
}

// MeasureAssignmentToolCache sums regular-file sizes under
// assignment-<ID>/tool-data/assignment without following agent-created
// symlinks and without traversing outside the Assignment subpath.
// Turn scratch under tool-data/turn is excluded by construction.
// Missing cache areas report zero usage. The measurement is read-only:
// it never changes permissions, retention, or cleanup state.
func (lifecycle *Lifecycle) MeasureAssignmentToolCache(ctx context.Context, assignmentID string) (ToolCacheUsage, error) {
	if lifecycle == nil || !uuidtext.Valid(assignmentID) {
		return ToolCacheUsage{}, ErrInvalidAssignmentID
	}
	root, err := lifecycle.toolDataRoot(assignmentID)
	if err != nil {
		return ToolCacheUsage{}, err
	}
	assignmentRoot := filepath.Dir(root)
	for _, parent := range []string{lifecycle.miseRoot, assignmentRoot} {
		exists, err := inspectOwnedDirectory(parent)
		if err != nil {
			return ToolCacheUsage{}, err
		}
		if !exists {
			return ToolCacheUsage{}, nil
		}
	}
	exists, err := inspectOwnedDirectory(root)
	if err != nil {
		return ToolCacheUsage{}, err
	}
	if !exists {
		return ToolCacheUsage{}, nil
	}
	cache := filepath.Join(root, "assignment")
	cacheDir, _, err := openDirectoryNoFollow(cache)
	if err != nil {
		if os.IsNotExist(err) {
			return ToolCacheUsage{}, nil
		}
		return ToolCacheUsage{}, fmt.Errorf("open Assignment tool cache: %w", err)
	}
	defer cacheDir.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(cacheDir.Fd()), &rootStat); err != nil {
		return ToolCacheUsage{}, fmt.Errorf("inspect Assignment tool cache: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(rootStat.Uid) != os.Geteuid() {
		return ToolCacheUsage{}, fmt.Errorf("%w: Assignment tool cache is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	rootDev := rootStat.Dev
	rootIno := rootStat.Ino
	seen := make(map[[16]byte]struct{})
	usage := ToolCacheUsage{}
	pending := []ownedDirectoryEntry{{path: "", dev: uint64(rootDev), ino: rootIno}}
	entries := 0
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return ToolCacheUsage{}, err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		directory := cacheDir
		owned := false
		if current.path != "" {
			reopened, err := reopenOwnedDirectory(cacheDir, current.path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return ToolCacheUsage{}, fmt.Errorf("reopen Assignment tool cache directory: %w", err)
			}
			directory = reopened
			owned = true
		}
		visitErr := func() error {
			var stat unix.Stat_t
			if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
				return err
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() ||
				uint64(stat.Dev) != current.dev || stat.Ino != current.ino {
				return fmt.Errorf("%w: Assignment tool cache directory changed", ErrUnsafeAssignmentPath)
			}
			if uint64(stat.Dev) != uint64(rootDev) {
				return fmt.Errorf("%w: Assignment tool cache crossed devices", ErrUnsafeAssignmentPath)
			}
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				names, readErr := directory.Readdirnames(128)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return readErr
				}
				for _, name := range names {
					if err := ctx.Err(); err != nil {
						return err
					}
					var childStat unix.Stat_t
					if err := unix.Fstatat(int(directory.Fd()), name, &childStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
						if os.IsNotExist(err) {
							continue
						}
						return err
					}
					entries++
					if entries > maxToolCacheEntries {
						usage.Truncated = true
						return nil
					}
					switch childStat.Mode & unix.S_IFMT {
					case unix.S_IFDIR:
						// Never follow symlinks: Fstatat with NOFOLLOW plus
						// O_NOFOLLOW open below pins the exact inode.
						if int(childStat.Uid) != os.Geteuid() || uint64(childStat.Dev) != uint64(rootDev) {
							// Skip foreign-owned or cross-device directories
							// rather than traversing outside the cache.
							continue
						}
						childPath := name
						if current.path != "" {
							childPath = filepath.Join(current.path, name)
						}
						pending = append(pending, ownedDirectoryEntry{
							path: childPath, dev: uint64(childStat.Dev), ino: childStat.Ino,
						})
					case unix.S_IFREG:
						key := fileIdentity(childStat.Dev, childStat.Ino)
						if _, duplicate := seen[key]; duplicate {
							continue
						}
						seen[key] = struct{}{}
						size := int64(childStat.Size)
						if size < 0 {
							size = 0
						}
						if usage.Bytes > math.MaxInt64-size {
							usage.Bytes = math.MaxInt64
						} else {
							usage.Bytes += size
						}
						usage.Files++
					default:
						// Symlinks, sockets, fifos, and devices are ignored:
						// their targets are never followed and their sizes
						// never contribute to the cache signal.
						continue
					}
					if usage.Truncated {
						return nil
					}
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
				if usage.Truncated {
					return nil
				}
			}
		}()
		if owned {
			closeErr := directory.Close()
			if visitErr == nil {
				visitErr = closeErr
			}
		}
		if visitErr != nil {
			return ToolCacheUsage{}, visitErr
		}
		if usage.Truncated {
			return usage, nil
		}
	}
	return usage, nil
}

func fileIdentity(dev uint64, ino uint64) [16]byte {
	var key [16]byte
	for i := 0; i < 8; i++ {
		key[i] = byte(dev >> (8 * i))
		key[8+i] = byte(ino >> (8 * i))
	}
	return key
}

// ToolCacheMonitor emits a bounded soft operational signal when an
// Assignment's assignment-lifecycle cache crosses a configurable warning
// threshold. It never enforces a quota and never changes retention or
// cleanup behavior.
type ToolCacheMonitor struct {
	lifecycle      *Lifecycle
	thresholdBytes int64
	pollInterval   time.Duration
	maxAssignments int
	logger         *slog.Logger
	onError        func(error)

	mu         sync.Mutex
	lastWarned map[string]int64
}

// ToolCacheMonitorConfig bounds the soft cache-growth signal.
type ToolCacheMonitorConfig struct {
	ThresholdBytes int64
	PollInterval   time.Duration
	MaxAssignments int
	Logger         *slog.Logger
	OnError        func(error)
}

// NewToolCacheMonitor validates the soft-signal configuration. A nil logger
// disables emission while still reporting warned observations to callers.
func NewToolCacheMonitor(lifecycle *Lifecycle, config ToolCacheMonitorConfig) (*ToolCacheMonitor, error) {
	if lifecycle == nil {
		return nil, fmt.Errorf("%w: tool cache monitor lifecycle is nil", ErrInvalidOptions)
	}
	if config.ThresholdBytes <= 0 {
		return nil, fmt.Errorf("%w: tool cache warning threshold must be positive", ErrInvalidOptions)
	}
	if config.PollInterval < time.Microsecond || config.PollInterval > 365*24*time.Hour {
		return nil, fmt.Errorf("%w: tool cache poll interval is invalid", ErrInvalidOptions)
	}
	maxAssignments := config.MaxAssignments
	if maxAssignments <= 0 {
		maxAssignments = maxToolCacheAssignmentsPerPoll
	}
	if maxAssignments > 100_000 {
		return nil, fmt.Errorf("%w: tool cache assignment bound is invalid", ErrInvalidOptions)
	}
	return &ToolCacheMonitor{
		lifecycle:      lifecycle,
		thresholdBytes: config.ThresholdBytes,
		pollInterval:   config.PollInterval,
		maxAssignments: maxAssignments,
		logger:         config.Logger,
		onError:        config.OnError,
		lastWarned:     make(map[string]int64),
	}, nil
}

// Observe measures one Assignment's cache and emits at most one warning per
// distinct size at or above the threshold. Unchanged usage does not re-warn.
// Usage below the threshold clears prior state so a future crossing warns
// again. Measurement failures are returned and must not block Agent Turns.
func (monitor *ToolCacheMonitor) Observe(ctx context.Context, assignmentID string) (ToolCacheUsage, bool, error) {
	if monitor == nil || monitor.lifecycle == nil {
		return ToolCacheUsage{}, false, fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	if !uuidtext.Valid(assignmentID) {
		return ToolCacheUsage{}, false, ErrInvalidAssignmentID
	}
	usage, err := monitor.lifecycle.MeasureAssignmentToolCache(ctx, assignmentID)
	if err != nil {
		return ToolCacheUsage{}, false, err
	}
	if usage.Bytes < monitor.thresholdBytes {
		monitor.mu.Lock()
		delete(monitor.lastWarned, assignmentID)
		monitor.mu.Unlock()
		return usage, false, nil
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if last, warned := monitor.lastWarned[assignmentID]; warned && last == usage.Bytes {
		return usage, false, nil
	}
	monitor.lastWarned[assignmentID] = usage.Bytes
	if monitor.logger != nil {
		monitor.logger.Warn("Assignment tool cache exceeds warning threshold",
			"assignment_id", assignmentID,
			"size_bytes", usage.Bytes,
			"threshold_bytes", monitor.thresholdBytes)
	}
	return usage, true, nil
}

// Check scans at most maxAssignments Assignment caches with a per-Assignment
// timeout. Per-Assignment measurement failures are skipped without failing
// the scan; only listing failures are returned.
func (monitor *ToolCacheMonitor) Check(ctx context.Context) (checked int, warned int, err error) {
	if monitor == nil || monitor.lifecycle == nil {
		return 0, 0, fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	ids, err := monitor.lifecycle.assignmentToolCacheIDs(monitor.maxAssignments)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return checked, warned, err
		}
		assignmentCtx, cancel := context.WithTimeout(ctx, toolCachePerAssignmentTimeout)
		_, didWarn, observeErr := monitor.Observe(assignmentCtx, id)
		cancel()
		checked++
		if observeErr != nil {
			// Measurement is best-effort and read-only; a single
			// Assignment failure must not fail the scan or block turns.
			continue
		}
		if didWarn {
			warned++
		}
	}
	return checked, warned, nil
}

// Run polls until cancellation. It runs off the Agent Turn critical path so
// large caches never block prompt, launch, or cleanup work.
func (monitor *ToolCacheMonitor) Run(ctx context.Context) error {
	if monitor == nil {
		return fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	for {
		if _, _, err := monitor.Check(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if monitor.onError != nil {
				monitor.onError(err)
			}
		}
		timer := time.NewTimer(monitor.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (lifecycle *Lifecycle) assignmentToolCacheIDs(max int) ([]string, error) {
	if lifecycle == nil {
		return nil, fmt.Errorf("%w: nil lifecycle", ErrInvalidOptions)
	}
	if max <= 0 {
		return nil, nil
	}
	root, _, err := openDirectoryNoFollow(lifecycle.miseRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list Assignment tool caches: %w", err)
	}
	defer root.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil {
		return nil, fmt.Errorf("inspect Assignment tool cache root: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("%w: Assignment tool cache root is not a directory", ErrUnsafeAssignmentPath)
	}
	names, err := root.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list Assignment tool caches: %w", err)
	}
	ids := make([]string, 0)
	for _, name := range names {
		id, found := cutAssignmentPrefix(name)
		if !found || !uuidtext.Valid(id) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(root.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > max {
		ids = ids[:max]
	}
	return ids, nil
}

func cutAssignmentPrefix(name string) (string, bool) {
	const prefix = "assignment-"
	if len(name) <= len(prefix) {
		return "", false
	}
	if name[:len(prefix)] != prefix {
		return "", false
	}
	return name[len(prefix):], true
}
