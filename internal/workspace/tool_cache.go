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
	"strings"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

// ToolCacheWarningThresholdReason documents the default soft signal.
// Agent Participant tool-data caches on the disk-backed mise volume survive
// turns and are collected only with their Participant. A 1 GiB warning
// surfaces unusual growth without imposing a hard quota. The on-disk layout
// keeps the historical assignment-<ID> prefix.
const (
	maxToolCacheAssignmentsPerPoll = 2000
	toolCachePerAssignmentTimeout  = 30 * time.Second
)

var maxToolCacheEntries = 500_000

// ToolCacheUsage is the bounded operational measurement for one Agent
// Participant's assignment-lifecycle cache. Turn-lifecycle scratch is never
// included. Bytes reports allocated disk usage (filesystem blocks), not
// apparent file length, so many small files correctly reflect storage
// pressure.
type ToolCacheUsage struct {
	Bytes     int64
	Files     int64
	Truncated bool
}

// MeasureAssignmentToolCache sums allocated disk usage under
// assignment-<ID>/tool-data/assignment without following agent-created
// symlinks and without traversing outside the Participant subpath. The ID is
// the Agent Participant identity; assignment- is the existing storage prefix.
// Turn scratch under tool-data/turn is excluded by construction.
// Missing cache areas report zero usage. The measurement is read-only:
// it never changes permissions, retention, or cleanup state.
func (lifecycle *Lifecycle) MeasureAssignmentToolCache(ctx context.Context, participantID string) (ToolCacheUsage, error) {
	return lifecycle.measureAssignmentToolCache(ctx, participantID, 0)
}

func (lifecycle *Lifecycle) measureAssignmentToolCache(ctx context.Context, participantID string, rotation uint64) (ToolCacheUsage, error) {
	if lifecycle == nil || !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, ErrInvalidAssignmentID
	}
	root, err := lifecycle.toolDataRoot(participantID)
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
		return ToolCacheUsage{}, fmt.Errorf("open Participant tool cache: %w", err)
	}
	defer cacheDir.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(cacheDir.Fd()), &rootStat); err != nil {
		return ToolCacheUsage{}, fmt.Errorf("inspect Participant tool cache: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(rootStat.Uid) != os.Geteuid() {
		return ToolCacheUsage{}, fmt.Errorf("%w: Participant tool cache is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	rootDev := rootStat.Dev
	rootIno := rootStat.Ino
	seen := make(map[[16]byte]struct{})
	usage := ToolCacheUsage{}
	addAllocated(&usage, rootStat)
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
			reopened, err := reopenToolCacheDirectory(cacheDir, current.path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				if isToolCacheAccessError(err) {
					// Read-only skip: an unreadable nested directory
					// must not change modes and must not fail the
					// whole measurement. The partial sum is marked
					// incomplete so warning state is preserved.
					usage.Truncated = true
					continue
				}
				return ToolCacheUsage{}, fmt.Errorf("reopen Participant tool cache directory: %w", err)
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
				return fmt.Errorf("%w: Participant tool cache directory changed", ErrUnsafeAssignmentPath)
			}
			if uint64(stat.Dev) != uint64(rootDev) {
				return fmt.Errorf("%w: Participant tool cache crossed devices", ErrUnsafeAssignmentPath)
			}
			names, err := listToolCacheNames(ctx, directory)
			if err != nil {
				if isToolCacheAccessError(err) {
					usage.Truncated = true
					return nil
				}
				return err
			}
			if len(names) > 1 {
				rotateToolCacheNames(names, rotation)
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
					if isToolCacheAccessError(err) {
						usage.Truncated = true
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
					// Count owned directory allocation once when encountered;
					// directories cannot be hardlinked and symlinks are never
					// followed, so each inode is counted at most once.
					addAllocated(&usage, childStat)
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
					addAllocated(&usage, childStat)
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
			return nil
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

// allocatedDiskBytes reports filesystem block allocation for one stat result.
// Blocks counts 512-byte units independently of apparent length, so sparse
// files and small files reflect true storage pressure.
func allocatedDiskBytes(stat unix.Stat_t) int64 {
	if stat.Blocks < 0 {
		return 0
	}
	if stat.Blocks > math.MaxInt64/512 {
		return math.MaxInt64
	}
	return stat.Blocks * 512
}

func addAllocated(usage *ToolCacheUsage, stat unix.Stat_t) {
	allocated := allocatedDiskBytes(stat)
	if usage.Bytes > math.MaxInt64-allocated {
		usage.Bytes = math.MaxInt64
	} else {
		usage.Bytes += allocated
	}
}

// openToolCacheChildNoFollow pins one owned child directory without ever
// changing its mode. Unlike the cleanup opener, it never restores
// permissions: EACCES and other open failures are returned so the caller can
// skip the subtree while leaving modes untouched.
func openToolCacheChildNoFollow(parentFD int, name string, expected unix.Stat_t) (*os.File, error) {
	if expected.Mode&unix.S_IFMT != unix.S_IFDIR || int(expected.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("%w: Participant tool cache directory is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(fd), name)
	var opened unix.Stat_t
	if err := unix.Fstat(int(child.Fd()), &opened); err != nil {
		_ = child.Close()
		return nil, err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFDIR || int(opened.Uid) != os.Geteuid() ||
		opened.Dev != expected.Dev || opened.Ino != expected.Ino {
		_ = child.Close()
		return nil, ErrUnsafeAssignmentPath
	}
	return child, nil
}

// reopenToolCacheDirectory reopens a relative cache directory from its pinned
// root using only read-only, no-follow opens. No chmod is ever performed.
func reopenToolCacheDirectory(root *os.File, relative string) (*os.File, error) {
	parent := root
	owned := false
	defer func() {
		if owned && parent != root {
			_ = parent.Close()
		}
	}()
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		if name == "" || name == "." || name == ".." {
			return nil, ErrUnsafeAssignmentPath
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if owned && parent != root {
				_ = parent.Close()
				owned = false
			}
			return nil, err
		}
		child, err := openToolCacheChildNoFollow(int(parent.Fd()), name, stat)
		if owned && parent != root {
			_ = parent.Close()
			owned = false
		}
		if err != nil {
			return nil, err
		}
		parent = child
		owned = true
	}
	if !owned {
		return nil, fmt.Errorf("%w: empty Participant tool cache path", ErrUnsafeAssignmentPath)
	}
	owned = false
	return parent, nil
}

func isToolCacheAccessError(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

func listToolCacheNames(ctx context.Context, directory *os.File) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names, err := directory.Readdirnames(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func rotateToolCacheNames(names []string, rotation uint64) {
	if len(names) <= 1 || rotation == 0 {
		return
	}
	offset := int(rotation % uint64(len(names)))
	if offset == 0 {
		return
	}
	rotated := append(append([]string(nil), names[offset:]...), names[:offset]...)
	copy(names, rotated)
}

// ToolCacheMonitor emits a bounded soft operational signal when an Agent
// Participant's assignment-lifecycle cache crosses a configurable warning
// threshold. It never enforces a quota and never changes retention or
// cleanup behavior. The on-disk layout keeps the historical assignment-
// prefix.
type ToolCacheMonitor struct {
	lifecycle      *Lifecycle
	thresholdBytes int64
	pollInterval   time.Duration
	maxAssignments int
	logger         *slog.Logger
	onError        func(error)

	mu          sync.Mutex
	lastWarned  map[string]int64
	scanCursor  string
	dirRotation map[string]uint64
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
		dirRotation:    make(map[string]uint64),
	}, nil
}

// Observe measures one Participant's cache and emits at most one warning per
// distinct complete size at or above the threshold. Unchanged usage does not
// re-warn. Usage below the threshold clears prior state so a future crossing
// warns again. Incomplete (truncated or access-skipped) measurements never
// clear warning state: a partial below-threshold sum preserves prior state,
// and a partial at or above threshold warns at most once until a complete
// measurement updates state. Measurement failures are returned and must not
// block Agent Turns.
func (monitor *ToolCacheMonitor) Observe(ctx context.Context, participantID string) (ToolCacheUsage, bool, error) {
	if monitor == nil || monitor.lifecycle == nil {
		return ToolCacheUsage{}, false, fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	if !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, false, ErrInvalidAssignmentID
	}
	monitor.mu.Lock()
	rotation := monitor.dirRotation[participantID]
	monitor.dirRotation[participantID] = rotation + 1
	monitor.mu.Unlock()
	usage, err := monitor.lifecycle.measureAssignmentToolCache(ctx, participantID, rotation)
	if err != nil {
		return ToolCacheUsage{}, false, err
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if usage.Truncated {
		if usage.Bytes >= monitor.thresholdBytes {
			if _, warned := monitor.lastWarned[participantID]; warned {
				return usage, false, nil
			}
			monitor.lastWarned[participantID] = usage.Bytes
			if monitor.logger != nil {
				monitor.logger.Warn("Participant tool cache exceeds warning threshold",
					"agent_participant_id", participantID,
					"size_bytes", usage.Bytes,
					"threshold_bytes", monitor.thresholdBytes)
			}
			return usage, true, nil
		}
		return usage, false, nil
	}
	if usage.Bytes < monitor.thresholdBytes {
		delete(monitor.lastWarned, participantID)
		return usage, false, nil
	}
	if last, warned := monitor.lastWarned[participantID]; warned && last == usage.Bytes {
		return usage, false, nil
	}
	monitor.lastWarned[participantID] = usage.Bytes
	if monitor.logger != nil {
		monitor.logger.Warn("Participant tool cache exceeds warning threshold",
			"agent_participant_id", participantID,
			"size_bytes", usage.Bytes,
			"threshold_bytes", monitor.thresholdBytes)
	}
	return usage, true, nil
}

// Check scans at most maxAssignments Participant caches with a per-Participant
// timeout, rotating the bounded selection across polls so every retained
// cache is eventually measured. Per-Participant measurement failures are
// skipped without failing the scan; only listing failures are returned.
func (monitor *ToolCacheMonitor) Check(ctx context.Context) (checked int, warned int, err error) {
	if monitor == nil || monitor.lifecycle == nil {
		return 0, 0, fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	all, err := monitor.lifecycle.listAssignmentParticipantIDs()
	if err != nil {
		return 0, 0, err
	}
	selected := monitor.selectAssignments(all)
	for _, id := range selected {
		if err := ctx.Err(); err != nil {
			return checked, warned, err
		}
		assignmentCtx, cancel := context.WithTimeout(ctx, toolCachePerAssignmentTimeout)
		_, didWarn, observeErr := monitor.Observe(assignmentCtx, id)
		cancel()
		checked++
		if observeErr != nil {
			// Measurement is best-effort and read-only; a single
			// Participant failure must not fail the scan or block turns.
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

// selectAssignments returns the next bounded window in sorted order starting
// after the stored cursor, wrapping around. The cursor advances to the last
// returned ID so later polls rotate through all retained Participants.
func (monitor *ToolCacheMonitor) selectAssignments(all []string) []string {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	if len(all) == 0 || monitor.maxAssignments <= 0 {
		return nil
	}
	start := 0
	if monitor.scanCursor != "" {
		start = sort.SearchStrings(all, monitor.scanCursor)
		if start < len(all) && all[start] == monitor.scanCursor {
			start++
		}
		if start >= len(all) {
			start = 0
		}
	}
	selected := make([]string, 0, monitor.maxAssignments)
	for i := 0; i < len(all) && len(selected) < monitor.maxAssignments; i++ {
		selected = append(selected, all[(start+i)%len(all)])
	}
	if len(selected) > 0 {
		monitor.scanCursor = selected[len(selected)-1]
	}
	return selected
}

func (lifecycle *Lifecycle) listAssignmentParticipantIDs() ([]string, error) {
	if lifecycle == nil {
		return nil, fmt.Errorf("%w: nil lifecycle", ErrInvalidOptions)
	}
	root, _, err := openDirectoryNoFollow(lifecycle.miseRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list Participant tool caches: %w", err)
	}
	defer root.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil {
		return nil, fmt.Errorf("inspect Participant tool cache root: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("%w: Participant tool cache root is not a directory", ErrUnsafeAssignmentPath)
	}
	names, err := root.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list Participant tool caches: %w", err)
	}
	ids := make([]string, 0, len(names))
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
