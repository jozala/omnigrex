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
	toolCacheDirBatchSize          = 128
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
	usage, _, err := lifecycle.measureWithCursors(ctx, participantID, nil)
	return usage, err
}

// measureWithCursors performs one bounded poll starting at the given
// per-directory offsets (relative dir path -> start index in readdir order).
// It returns the partial usage for this poll and the updated offsets for the
// next poll. Offsets advance only based on their own directory's progress, so
// every entry is eventually enumerated regardless of tree shape. Enumeration
// is batched (never materializing whole directories) and bounded by the
// global entry budget.
func (lifecycle *Lifecycle) measureWithCursors(ctx context.Context, participantID string, cursors map[string]int) (ToolCacheUsage, map[string]int, error) {
	if lifecycle == nil || !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, cursors, ErrInvalidAssignmentID
	}
	root, err := lifecycle.toolDataRoot(participantID)
	if err != nil {
		return ToolCacheUsage{}, cursors, err
	}
	assignmentRoot := filepath.Dir(root)
	for _, parent := range []string{lifecycle.miseRoot, assignmentRoot} {
		exists, err := inspectOwnedDirectory(parent)
		if err != nil {
			return ToolCacheUsage{}, cursors, err
		}
		if !exists {
			return ToolCacheUsage{}, cursors, nil
		}
	}
	exists, err := inspectOwnedDirectory(root)
	if err != nil {
		return ToolCacheUsage{}, cursors, err
	}
	if !exists {
		return ToolCacheUsage{}, cursors, nil
	}
	cache := filepath.Join(root, "assignment")
	cacheDir, _, err := openDirectoryNoFollow(cache)
	if err != nil {
		if os.IsNotExist(err) {
			return ToolCacheUsage{}, cursors, nil
		}
		return ToolCacheUsage{}, cursors, fmt.Errorf("open Participant tool cache: %w", err)
	}
	defer cacheDir.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(cacheDir.Fd()), &rootStat); err != nil {
		return ToolCacheUsage{}, cursors, fmt.Errorf("inspect Participant tool cache: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(rootStat.Uid) != os.Geteuid() {
		return ToolCacheUsage{}, cursors, fmt.Errorf("%w: Participant tool cache is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	rootDev := uint64(rootStat.Dev)
	rootIno := uint64(rootStat.Ino)
	if cursors == nil {
		cursors = make(map[string]int)
	}
	nextCursors := make(map[string]int, len(cursors)+8)
	for k, v := range cursors {
		if v >= 0 {
			nextCursors[k] = v
		}
	}
	seen := make(map[[16]byte]struct{})
	usage := ToolCacheUsage{}
	addAllocated(&usage, rootStat)
	pending := []ownedDirectoryEntry{{path: "", dev: rootDev, ino: rootIno}}
	entries := 0
	truncated := false
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return ToolCacheUsage{}, cursors, err
		}
		if entries >= maxToolCacheEntries {
			truncated = true
			break
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		completed, visitErr := lifecycle.visitToolCacheDir(ctx, cacheDir, rootDev, current, nextCursors, seen, &usage, &entries, &pending)
		if visitErr != nil {
			return ToolCacheUsage{}, cursors, visitErr
		}
		if !completed {
			truncated = true
			break
		}
	}
	usage.Truncated = usage.Truncated || truncated || entries > maxToolCacheEntries
	return usage, nextCursors, nil
}

// visitToolCacheDir processes one directory starting at its persisted offset,
// in bounded batches, wrapping to the prefix when the suffix completes within
// budget. It updates nextCursors for dirPath: reset to 0 when fully
// enumerated, otherwise set to the resume offset. It returns completed=false
// when the global budget is exhausted (truncated) or when an access skip
// marks the overall scan incomplete. Pending subdirectories are queued for
// depth-first traversal.
func (lifecycle *Lifecycle) visitToolCacheDir(ctx context.Context, cacheDir *os.File, rootDev uint64, current ownedDirectoryEntry, nextCursors map[string]int, seen map[[16]byte]struct{}, usage *ToolCacheUsage, entries *int, pending *[]ownedDirectoryEntry) (bool, error) {
	dirPath := current.path
	startOffset := nextCursors[dirPath]
	if startOffset < 0 {
		startOffset = 0
	}
	openDir := func() (*os.File, bool, error) {
		if dirPath == "" {
			// Reopen the root from its path so prefix wraps use a fresh
			// listing position without relying on directory Seek semantics.
			// cacheDir itself is kept open by the caller; duplicate a fresh
			// fd for position-independent enumeration.
			dup, err := reopenToolCacheDirectorySelf(cacheDir)
			if err != nil {
				return nil, false, err
			}
			return dup, true, nil
		}
		reopened, err := reopenToolCacheDirectory(cacheDir, dirPath)
		return reopened, true, err
	}
	// Phase 1: suffix from startOffset to EOF.
	directory, owned, err := openDir()
	if err != nil {
		if os.IsNotExist(err) {
			delete(nextCursors, dirPath)
			return true, nil
		}
		if isToolCacheAccessError(err) {
			usage.Truncated = true
			return true, nil
		}
		return false, fmt.Errorf("reopen Participant tool cache directory: %w", err)
	}
	closeDir := func() {
		if owned && directory != nil && directory != cacheDir {
			_ = directory.Close()
		}
	}
	defer closeDir()
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() ||
		uint64(stat.Dev) != current.dev || uint64(stat.Ino) != current.ino {
		return false, fmt.Errorf("%w: Participant tool cache directory changed", ErrUnsafeAssignmentPath)
	}
	if uint64(stat.Dev) != rootDev {
		return false, fmt.Errorf("%w: Participant tool cache crossed devices", ErrUnsafeAssignmentPath)
	}
	processedSuffix, suffixDone, err := processToolCacheBatchRange(ctx, directory, startOffset, -1, rootDev, current, nextCursors, seen, usage, entries, pending)
	if err != nil {
		return false, err
	}
	if !suffixDone {
		// Budget exhausted mid-suffix; resume here next poll.
		nextCursors[dirPath] = startOffset + processedSuffix
		return false, nil
	}
	if startOffset == 0 {
		nextCursors[dirPath] = 0
		return true, nil
	}
	// Phase 2: prefix 0..startOffset-1 to complete full enumeration within
	// budget when possible.
	_ = directory.Close()
	directory = nil
	fresh, _, err := openDir()
	if err != nil {
		if os.IsNotExist(err) {
			delete(nextCursors, dirPath)
			return true, nil
		}
		if isToolCacheAccessError(err) {
			// Suffix was fully enumerated; prefix access failure still
			// leaves the scan incomplete but preserves suffix progress.
			usage.Truncated = true
			nextCursors[dirPath] = 0
			return true, nil
		}
		return false, fmt.Errorf("reopen Participant tool cache directory: %w", err)
	}
	directory = fresh
	defer func() {
		if directory != nil && directory != cacheDir {
			_ = directory.Close()
		}
	}()
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() ||
		uint64(stat.Dev) != current.dev || uint64(stat.Ino) != current.ino {
		return false, fmt.Errorf("%w: Participant tool cache directory changed", ErrUnsafeAssignmentPath)
	}
	processedPrefix, prefixDone, err := processToolCacheBatchRange(ctx, directory, 0, startOffset, rootDev, current, nextCursors, seen, usage, entries, pending)
	if err != nil {
		return false, err
	}
	_ = processedSuffix
	if !prefixDone {
		// Budget exhausted within prefix; resume inside prefix next poll.
		nextCursors[dirPath] = processedPrefix
		return false, nil
	}
	nextCursors[dirPath] = 0
	return true, nil
}

// processToolCacheBatchRange enumerates one listing pass in bounded batches.
// Starting at skip offsets in readdir order, it processes up to limit entries
// (limit < 0 means until EOF or global budget). It returns processed count
// and whether the pass reached EOF without hitting the global budget.
func processToolCacheBatchRange(ctx context.Context, directory *os.File, skip int, limit int, rootDev uint64, current ownedDirectoryEntry, nextCursors map[string]int, seen map[[16]byte]struct{}, usage *ToolCacheUsage, entries *int, pending *[]ownedDirectoryEntry) (int, bool, error) {
	skipped := 0
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return processed, false, err
		}
		if *entries >= maxToolCacheEntries {
			return processed, false, nil
		}
		if limit >= 0 && processed >= limit {
			// Prefix limit reached; pass complete (caller decides wrap).
			// Drain to EOF to distinguish fully-enumerated vs truncated?
			// For prefix passes limit==startOffset bounds the prefix length,
			// reaching limit means prefix fully processed.
			return processed, true, nil
		}
		names, readErr := directory.Readdirnames(toolCacheDirBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if isToolCacheAccessError(readErr) {
				usage.Truncated = true
				return processed, true, nil
			}
			return processed, false, readErr
		}
		for _, name := range names {
			if skipped < skip {
				skipped++
				continue
			}
			if limit >= 0 && processed >= limit {
				break
			}
			if *entries >= maxToolCacheEntries {
				return processed, false, nil
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
				return processed, false, err
			}
			*entries++
			if *entries > maxToolCacheEntries {
				return processed, false, nil
			}
			switch childStat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				if int(childStat.Uid) != os.Geteuid() || uint64(childStat.Dev) != rootDev {
					continue
				}
				addAllocated(usage, childStat)
				childPath := name
				if current.path != "" {
					childPath = filepath.Join(current.path, name)
				}
				*pending = append(*pending, ownedDirectoryEntry{
					path: childPath, dev: uint64(childStat.Dev), ino: uint64(childStat.Ino),
				})
			case unix.S_IFREG:
				key := fileIdentity(uint64(childStat.Dev), uint64(childStat.Ino))
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				addAllocated(usage, childStat)
				usage.Files++
			default:
				continue
			}
			processed++
			if *entries >= maxToolCacheEntries {
				return processed, false, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return processed, true, nil
		}
		if len(names) == 0 {
			return processed, true, nil
		}
	}
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

// reopenToolCacheDirectorySelf duplicates a fresh listing fd for an already
// open cache directory without relying on directory Seek semantics.
func reopenToolCacheDirectorySelf(dir *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	dup := os.NewFile(uintptr(fd), dir.Name())
	var a, b unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &a); err != nil {
		_ = dup.Close()
		return nil, err
	}
	if err := unix.Fstat(int(dup.Fd()), &b); err != nil {
		_ = dup.Close()
		return nil, err
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		_ = dup.Close()
		return nil, ErrUnsafeAssignmentPath
	}
	return dup, nil
}

func isToolCacheAccessError(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
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

	mu         sync.Mutex
	lastWarned map[string]int64
	scanCursor string
	dirCursors map[string]map[string]int
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
		dirCursors:     make(map[string]map[string]int),
	}, nil
}

// Observe measures one Participant's cache and emits at most one warning per
// distinct complete size at or above the threshold. Unchanged usage does not
// re-warn. Usage below the threshold clears prior state so a future crossing
// warns again. Incomplete (truncated or access-skipped) measurements never
// clear warning state: a partial below-threshold sum preserves prior state,
// and a partial at or above threshold warns at most once until a complete
// measurement updates state. Per-directory cursors persist across polls so
// bounded scans eventually enumerate every entry. Measurement failures are
// returned and must not block Agent Turns.
func (monitor *ToolCacheMonitor) Observe(ctx context.Context, participantID string) (ToolCacheUsage, bool, error) {
	if monitor == nil || monitor.lifecycle == nil {
		return ToolCacheUsage{}, false, fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	if !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, false, ErrInvalidAssignmentID
	}
	monitor.mu.Lock()
	cursors := make(map[string]int, len(monitor.dirCursors[participantID])+4)
	for k, v := range monitor.dirCursors[participantID] {
		if v >= 0 {
			cursors[k] = v
		}
	}
	monitor.mu.Unlock()
	usage, updated, err := monitor.lifecycle.measureWithCursors(ctx, participantID, cursors)
	if err != nil {
		return ToolCacheUsage{}, false, err
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	monitor.dirCursors[participantID] = updated
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
	// Complete scans reset per-directory cursors for the next cycle.
	monitor.dirCursors[participantID] = make(map[string]int)
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
