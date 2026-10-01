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

const (
	maxToolCacheAssignmentsPerPoll = 2000
	toolCachePerAssignmentTimeout  = 30 * time.Second
	toolCacheDirBatchSize          = 128
)

var maxToolCacheEntries = 500_000

type ToolCacheUsage struct {
	Bytes     int64
	Files     int64
	Truncated bool
}

func (lifecycle *Lifecycle) MeasureAssignmentToolCache(ctx context.Context, participantID string) (ToolCacheUsage, error) {
	usage, _, _, _, _, _, err := lifecycle.measureCyclePoll(ctx, participantID, nil, nil, 0, 0, false)
	return usage, err
}

// participantScan persists the exploration frontier across polls so completed
// regions are never re-enumerated and the frontier advances monotonically.
// Pending holds remaining directories in the current cycle (stack, LIFO for
// depth-first). Cursors holds resume offsets per directory path for partially
// processed directories. Accum sums disjoint subsets visited so far in the
// current cycle (no double-count, since each entry is visited once per cycle).
// HadSkip marks access skips (lower bound). Warned marks whether this cycle
// already warned (suppress further warnings in same cycle).
type participantScan struct {
	pending    []ownedDirectoryEntry
	cursors    map[string]int
	accumBytes int64
	accumFiles int64
	hadSkip    bool
	warned     bool
	started    bool
}

func (lifecycle *Lifecycle) measureCyclePoll(ctx context.Context, participantID string, pending []ownedDirectoryEntry, cursors map[string]int, accumBytes, accumFiles int64, hadSkip bool) (ToolCacheUsage, []ownedDirectoryEntry, map[string]int, int64, int64, bool, error) {
	if lifecycle == nil || !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, ErrInvalidAssignmentID
	}
	root, err := lifecycle.toolDataRoot(participantID)
	if err != nil {
		return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, err
	}
	assignmentRoot := filepath.Dir(root)
	for _, parent := range []string{lifecycle.miseRoot, assignmentRoot} {
		exists, err := inspectOwnedDirectory(parent)
		if err != nil {
			return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, err
		}
		if !exists {
			return ToolCacheUsage{}, nil, make(map[string]int), 0, 0, false, nil
		}
	}
	exists, err := inspectOwnedDirectory(root)
	if err != nil {
		return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, err
	}
	if !exists {
		return ToolCacheUsage{}, nil, make(map[string]int), 0, 0, false, nil
	}
	cache := filepath.Join(root, "assignment")
	cacheDir, _, err := openDirectoryNoFollow(cache)
	if err != nil {
		if os.IsNotExist(err) {
			return ToolCacheUsage{}, nil, make(map[string]int), 0, 0, false, nil
		}
		return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, fmt.Errorf("open Participant tool cache: %w", err)
	}
	defer cacheDir.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(cacheDir.Fd()), &rootStat); err != nil {
		return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, fmt.Errorf("inspect Participant tool cache: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(rootStat.Uid) != os.Geteuid() {
		return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, fmt.Errorf("%w: Participant tool cache is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	rootDev := uint64(rootStat.Dev)
	rootIno := uint64(rootStat.Ino)
	if cursors == nil {
		cursors = make(map[string]int)
	}
	// Fresh cycle: initialize frontier with root, count root allocation once.
	isFresh := len(pending) == 0 && accumBytes == 0 && accumFiles == 0 && len(cursors) == 0 && !hadSkip
	usage := ToolCacheUsage{}
	// Accum holds disjoint sum so far in cycle (excluding this poll's new).
	// This poll adds new entries to accum; usage for warning is accum total.
	// For fresh cycles, count root allocation once.
	if isFresh {
		addAllocated(&usage, rootStat)
		accumBytes += usage.Bytes
		// Files: root dir itself not counted as file.
		pending = []ownedDirectoryEntry{{path: "", dev: rootDev, ino: rootIno}}
	} else {
		// Resumed cycle: usage starts from 0 for this poll's new entries;
		// accum holds prior polls' sum, to which we add.
		usage.Bytes = 0
		usage.Files = 0
	}
	// Track per-poll seen for hardlink dedup within this poll only.
	// Cross-poll hardlink splits across disjoint subsets could double-count
	// (overestimate, safe direction for soft signal); accepted as best-effort.
	seen := make(map[[16]byte]struct{})
	entries := 0
	// entries counts processed entries in this poll only (for per-poll budget).
	// Accum counts bytes/files across polls in cycle (disjoint, no double-count
	// since each entry visited once per cycle via cursors+frontier persistence).
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, err
		}
		if entries > maxToolCacheEntries {
			break
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		completed, visitErr := lifecycle.visitToolCacheDirOnce(ctx, cacheDir, rootDev, current, cursors, seen, &usage, &entries, &pending, &accumBytes, &accumFiles, &hadSkip)
		if visitErr != nil {
			return ToolCacheUsage{}, pending, cursors, accumBytes, accumFiles, hadSkip, visitErr
		}
		if !completed {
			// Budget exhausted mid-dir or access pause; frontier saved for
			// next poll (current dir already pushed back with updated cursor
			// by visit helper when truncated due to budget; for access skips
			// visit continues with next pending, not break? Actually visit
			// returns completed=false only on budget exhaustion (truncated),
			// access skips return completed=true with hadSkip set (continue).
			// So !completed here means budget exhausted, stop poll.
			break
		}
	}
	// Poll partial usage for this poll is in usage (new entries this poll).
	// Cycle accum total so far = accumBytes/accumFiles (including this poll,
	// since visit added directly to accum? Actually visit adds to usage and
	// accum? Let's have visit add to both usage (poll partial) and accum
	// (cycle total). Currently visit adds to usage only; need to also add to
	// accum. Simpler: after poll, accum total = accumBytes (updated by visit
	// via pointers) + usage? No, double-count. Let's have visit add to accum
	// directly via pointers, and usage tracks poll partial separately? Actually
	// visit currently adds to usage only. Change to add to both: usage (poll)
	// and accum (cycle). Implement by passing accum pointers and adding in
	// visit when counting dirs/files.
	// For now, to avoid confusion, compute cycle total as accumBytes/accumFiles
	// updated by visit (visit adds to accum directly). Poll usage for return
	// (for Measure compat, single poll from fresh cycle, accum==poll partial
	// plus root? Root already added to accum at cycle start, poll partial
	// includes root? Fresh cycle: accum starts 0, root added to usage and accum
	// (both). Good.
	// Truncated when frontier non-empty (more work remains in cycle) or entries
	// budget hit.
	truncated := len(pending) != 0
	// Boundary: exactly-budget with frontier empty is complete (not truncated).
	// entries==max with pending empty means all work done within budget exactly.
	// Our loop breaks on entries>max (strict), not >=, so exactly-max with
	// pending empty completes. Good (see visit/process strict checks).
	cycleUsage := ToolCacheUsage{Bytes: accumBytes, Files: accumFiles, Truncated: truncated || hadSkip}
	// Poll usage for Measure compat (single poll fresh cycle): return cycleUsage
	// when fresh? Actually Measure calls with fresh (nil frontier), single poll
	// up to budget, returns cycleUsage (accum total after poll, which equals
	// poll partial when fresh since accum started 0). Good.
	_ = isFresh
	return cycleUsage, pending, cursors, accumBytes, accumFiles, hadSkip, nil
}

// visitToolCacheDirOnce processes one directory from its persisted offset to
// EOF (no wrap, since prefix already counted in prior polls of same cycle for
// resumed dirs, and fresh dirs start at 0 covering all). It updates cursors:
// delete on full enumeration, set resume offset on budget truncation. It adds
// newly visited dirs/files to usage (poll partial) and accum (cycle total).
// It returns completed=false only when global budget is exhausted (truncated,
// frontier saved for next poll, current dir pushed back with updated cursor).
// Access skips mark hadSkip and return completed=true (continue with next
// pending, not break entire poll).
func (lifecycle *Lifecycle) visitToolCacheDirOnce(ctx context.Context, cacheDir *os.File, rootDev uint64, current ownedDirectoryEntry, cursors map[string]int, seen map[[16]byte]struct{}, usage *ToolCacheUsage, entries *int, pending *[]ownedDirectoryEntry, accumBytes, accumFiles *int64, hadSkip *bool) (bool, error) {
	dirPath := current.path
	startOffset := cursors[dirPath]
	if startOffset < 0 {
		startOffset = 0
	}
	var directory *os.File
	var owned bool
	if dirPath == "" {
		dup, err := reopenToolCacheDirectorySelf(cacheDir)
		if err != nil {
			if os.IsNotExist(err) {
				delete(cursors, dirPath)
				return true, nil
			}
			if isToolCacheAccessError(err) {
				*hadSkip = true
				usage.Truncated = true
				return true, nil
			}
			return false, fmt.Errorf("reopen Participant tool cache directory: %w", err)
		}
		directory = dup
		owned = true
	} else {
		reopened, err := reopenToolCacheDirectory(cacheDir, dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				delete(cursors, dirPath)
				return true, nil
			}
			if isToolCacheAccessError(err) {
				*hadSkip = true
				usage.Truncated = true
				return true, nil
			}
			return false, fmt.Errorf("reopen Participant tool cache directory: %w", err)
		}
		directory = reopened
		owned = true
	}
	defer func() {
		if owned && directory != nil && directory != cacheDir {
			_ = directory.Close()
		}
	}()
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
	// Batched enumeration from startOffset, no wrap, up to global budget.
	// streamed tracks the readdir stream position past the skip window for the
	// resume cursor, including ignored and access-skipped names (to avoid
	// repinning on the same block every poll). Vanished (ENOENT) names are not
	// counted since the next listing will not contain them.
	skipped := 0
	streamed := 0
	for {
		if err := ctx.Err(); err != nil {
			cursors[dirPath] = startOffset + streamed
			*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
			return false, err
		}
		if *entries > maxToolCacheEntries {
			// Budget exhausted: save resume cursor, push dir back for next poll.
			cursors[dirPath] = startOffset + streamed
			*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
			return false, nil
		}
		names, readErr := directory.Readdirnames(toolCacheDirBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if isToolCacheAccessError(readErr) {
				*hadSkip = true
				usage.Truncated = true
				delete(cursors, dirPath)
				return true, nil
			}
			cursors[dirPath] = startOffset + streamed
			*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
			return false, readErr
		}
		for _, name := range names {
			if skipped < startOffset {
				skipped++
				continue
			}
			if *entries > maxToolCacheEntries {
				cursors[dirPath] = startOffset + streamed
				*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
				return false, nil
			}
			var childStat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), name, &childStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				if isToolCacheAccessError(err) {
					*hadSkip = true
					usage.Truncated = true
					streamed++
					continue
				}
				cursors[dirPath] = startOffset + streamed
				*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
				return false, err
			}
			*entries++
			if *entries > maxToolCacheEntries {
				// Budget hit on this entry: do not count it this poll, resume
				// at this entry next poll.
				// entries already incremented for budget accounting; streamed
				// does not advance for this uncounted entry so it is retried.
				cursors[dirPath] = startOffset + streamed
				*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
				return false, nil
			}
			switch childStat.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				if int(childStat.Uid) != os.Geteuid() || uint64(childStat.Dev) != rootDev {
					streamed++
					continue
				}
				addAllocated(usage, childStat)
				*accumBytes += allocatedDiskBytes(childStat)
				childPath := name
				if current.path != "" {
					childPath = filepath.Join(current.path, name)
				}
				*pending = append(*pending, ownedDirectoryEntry{
					path: childPath, dev: uint64(childStat.Dev), ino: uint64(childStat.Ino),
				})
				streamed++
			case unix.S_IFREG:
				key := fileIdentity(uint64(childStat.Dev), uint64(childStat.Ino))
				if _, duplicate := seen[key]; duplicate {
					streamed++
					continue
				}
				seen[key] = struct{}{}
				addAllocated(usage, childStat)
				*accumBytes += allocatedDiskBytes(childStat)
				usage.Files++
				*accumFiles++
				streamed++
			default:
				streamed++
				continue
			}
			if *entries > maxToolCacheEntries {
				cursors[dirPath] = startOffset + streamed
				*pending = append(*pending, ownedDirectoryEntry{path: dirPath, dev: current.dev, ino: current.ino})
				return false, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			delete(cursors, dirPath)
			return true, nil
		}
		if len(names) == 0 {
			delete(cursors, dirPath)
			return true, nil
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
	scans      map[string]*participantScanState
}

type participantScanState struct {
	pending    []ownedDirectoryEntry
	cursors    map[string]int
	accumBytes int64
	accumFiles int64
	hadSkip    bool
	warned     bool
}

type ToolCacheMonitorConfig struct {
	ThresholdBytes int64
	PollInterval   time.Duration
	MaxAssignments int
	Logger         *slog.Logger
	OnError        func(error)
}

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
		scans:          make(map[string]*participantScanState),
	}, nil
}

func (monitor *ToolCacheMonitor) Observe(ctx context.Context, participantID string) (ToolCacheUsage, bool, error) {
	if monitor == nil || monitor.lifecycle == nil {
		return ToolCacheUsage{}, false, fmt.Errorf("%w: tool cache monitor is nil", ErrInvalidOptions)
	}
	if !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, false, ErrInvalidAssignmentID
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	scan, ok := monitor.scans[participantID]
	if !ok || scan == nil {
		scan = &participantScanState{cursors: make(map[string]int)}
		monitor.scans[participantID] = scan
	}
	// One bounded poll continuing the persisted frontier (or starting fresh).
	// Adopt frontier and accumulation even when the poll ends in error so
	// interrupted scans resume instead of restarting.
	usage, pending, cursors, accumBytes, accumFiles, hadSkip, err := monitor.lifecycle.measureCyclePoll(ctx, participantID, scan.pending, scan.cursors, scan.accumBytes, scan.accumFiles, scan.hadSkip)
	scan.pending = pending
	scan.cursors = cursors
	scan.accumBytes = accumBytes
	scan.accumFiles = accumFiles
	scan.hadSkip = hadSkip
	if err != nil {
		// Progress made before interruption is preserved in scan state above;
		// report the error without warning so the next poll resumes.
		_ = usage
		return ToolCacheUsage{Bytes: accumBytes, Files: accumFiles, Truncated: true}, false, err
	}
	cycleComplete := len(pending) == 0
	// Cycle total so far (lower bound when incomplete, exact when complete
	// without skips).
	total := ToolCacheUsage{Bytes: accumBytes, Files: accumFiles, Truncated: !cycleComplete || hadSkip}
	if !cycleComplete {
		// Incomplete cycle: partial sums are lower bounds; suppress stale
		// partials at or below the warned watermark to avoid repeated noise.
		if total.Bytes >= monitor.thresholdBytes {
			if scan.warned {
				return total, false, nil
			}
			if last, warned := monitor.lastWarned[participantID]; warned && total.Bytes <= last {
				scan.warned = true
				return total, false, nil
			}
			scan.warned = true
			monitor.lastWarned[participantID] = total.Bytes
			if monitor.logger != nil {
				monitor.logger.Warn("Participant tool cache exceeds warning threshold",
					"agent_participant_id", participantID,
					"size_bytes", total.Bytes,
					"threshold_bytes", monitor.thresholdBytes)
			}
			return total, true, nil
		}
		return total, false, nil
	}
	// Cycle complete: evaluate exact total (or lower bound if hadSkip).
	defer func() {
		// Reset for next cycle (fresh frontier), keep lastWarned/cycle semantics.
		monitor.scans[participantID] = &participantScanState{cursors: make(map[string]int)}
	}()
	if hadSkip {
		if total.Bytes >= monitor.thresholdBytes {
			if scan.warned {
				return total, false, nil
			}
			if last, warned := monitor.lastWarned[participantID]; warned && total.Bytes <= last {
				return total, false, nil
			}
			monitor.lastWarned[participantID] = total.Bytes
			if monitor.logger != nil {
				monitor.logger.Warn("Participant tool cache exceeds warning threshold",
					"agent_participant_id", participantID,
					"size_bytes", total.Bytes,
					"threshold_bytes", monitor.thresholdBytes)
			}
			return total, true, nil
		}
		return total, false, nil
	}
	if total.Bytes < monitor.thresholdBytes {
		delete(monitor.lastWarned, participantID)
		return total, false, nil
	}
	if scan.warned {
		return total, false, nil
	}
	if last, warned := monitor.lastWarned[participantID]; warned && last == total.Bytes {
		return total, false, nil
	}
	monitor.lastWarned[participantID] = total.Bytes
	if monitor.logger != nil {
		monitor.logger.Warn("Participant tool cache exceeds warning threshold",
			"agent_participant_id", participantID,
			"size_bytes", total.Bytes,
			"threshold_bytes", monitor.thresholdBytes)
	}
	// usage for compat (single-poll callers expect poll partial? For cycle
	// complete via multiple polls, total is exact total, correct to return.
	_ = usage
	return total, true, nil
}

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
