package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

const (
	maxToolCacheEntries            = 500_000
	maxToolCacheAssignmentsPerPoll = 2000
	maxActiveToolCacheScans        = 4
	toolCachePerAssignmentTimeout  = 30 * time.Second
)

var errToolCacheScanLimit = errors.New("active tool cache scan limit reached")

// ToolCacheUsage reports allocated bytes, excluding turn scratch and symlinks.
// Truncated means unfinished traversal or skipped entries, not an exact total.
type ToolCacheUsage struct {
	Bytes     int64
	Files     int64
	Truncated bool
}

// MeasureAssignmentToolCache performs one bounded, read-only observation.
// The historical storage prefix names an Agent Participant, not a Stage Assignment.
func (lifecycle *Lifecycle) MeasureAssignmentToolCache(ctx context.Context, participantID string) (ToolCacheUsage, error) {
	if err := ctx.Err(); err != nil {
		return ToolCacheUsage{Truncated: true}, err
	}
	directory, stat, err := lifecycle.openToolCacheRoot(participantID)
	if err != nil || directory == nil {
		return ToolCacheUsage{}, err
	}
	scan := newToolCacheScan(directory, stat)
	defer scan.Close()
	return scan.Step(ctx, maxToolCacheEntries)
}

type ToolCacheMonitor struct {
	lifecycle      *Lifecycle
	thresholdBytes int64
	pollInterval   time.Duration
	maxAssignments int
	entryBudget    int
	logger         *slog.Logger
	onError        func(error)

	mu         sync.Mutex
	lastWarned map[string]int64
	scanCursor string
	scans      map[string]*monitoredCacheScan
}

type monitoredCacheScan struct {
	*toolCacheScan
	warned bool
}

type ToolCacheMonitorConfig struct {
	ThresholdBytes int64
	PollInterval   time.Duration
	MaxAssignments int
	Logger         *slog.Logger
	OnError        func(error)
}

func NewToolCacheMonitor(lifecycle *Lifecycle, config ToolCacheMonitorConfig) (*ToolCacheMonitor, error) {
	if lifecycle == nil || config.ThresholdBytes <= 0 ||
		config.PollInterval < time.Microsecond || config.PollInterval > 365*24*time.Hour ||
		config.MaxAssignments > 100_000 {
		return nil, fmt.Errorf("%w: invalid tool cache monitor configuration", ErrInvalidOptions)
	}
	maxAssignments := config.MaxAssignments
	if maxAssignments <= 0 {
		maxAssignments = maxToolCacheAssignmentsPerPoll
	}
	return &ToolCacheMonitor{
		lifecycle: lifecycle, thresholdBytes: config.ThresholdBytes,
		pollInterval: config.PollInterval, maxAssignments: maxAssignments,
		entryBudget: maxToolCacheEntries, logger: config.Logger, onError: config.OnError,
		lastWarned: make(map[string]int64), scans: make(map[string]*monitoredCacheScan),
	}, nil
}

// Observe yields at its budget or deadline without closing the directory streams.
// Fatal failures, root replacement, and completed scans release their resources.
func (monitor *ToolCacheMonitor) Observe(ctx context.Context, participantID string) (ToolCacheUsage, bool, error) {
	if monitor == nil || monitor.lifecycle == nil {
		return ToolCacheUsage{}, false, fmt.Errorf("%w: nil tool cache monitor", ErrInvalidOptions)
	}
	if !uuidtext.Valid(participantID) {
		return ToolCacheUsage{}, false, ErrInvalidAssignmentID
	}
	if err := ctx.Err(); err != nil {
		return ToolCacheUsage{Truncated: true}, false, err
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	directory, stat, err := monitor.lifecycle.openToolCacheRoot(participantID)
	if err != nil {
		monitor.dropScan(participantID)
		return ToolCacheUsage{Truncated: true}, false, err
	}
	if directory == nil {
		monitor.dropScan(participantID)
		delete(monitor.lastWarned, participantID)
		return ToolCacheUsage{}, false, nil
	}
	scan := monitor.scans[participantID]
	if scan != nil && scan.root != cacheIdentity(stat) {
		monitor.dropScan(participantID)
		delete(monitor.lastWarned, participantID)
		scan = nil
	}
	if scan == nil {
		if len(monitor.scans) >= maxActiveToolCacheScans {
			_ = directory.Close()
			return ToolCacheUsage{Truncated: true}, false, errToolCacheScanLimit
		}
		scan = &monitoredCacheScan{toolCacheScan: newToolCacheScan(directory, stat)}
		monitor.scans[participantID] = scan
	} else {
		_ = directory.Close() // Identity check only; the iterator stays open.
	}
	usage, err := scan.Step(ctx, monitor.entryBudget)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return usage, monitor.recordUsage(participantID, scan, usage), err
		}
		monitor.dropScan(participantID)
		return usage, false, err
	}
	warned := monitor.recordUsage(participantID, scan, usage)
	if len(scan.frames) == 0 {
		monitor.dropScan(participantID)
	}
	return usage, warned, nil
}

func (monitor *ToolCacheMonitor) recordUsage(id string, scan *monitoredCacheScan, usage ToolCacheUsage) bool {
	complete := !usage.Truncated
	if usage.Bytes < monitor.thresholdBytes {
		if complete {
			delete(monitor.lastWarned, id)
		}
		return false
	}
	last, observed := monitor.lastWarned[id]
	warn := !scan.warned && (!observed || (complete && usage.Bytes != last) || usage.Bytes > last)
	if warn {
		scan.warned = true
		monitor.lastWarned[id] = usage.Bytes
		if monitor.logger != nil {
			monitor.logger.Warn("Agent Participant tool cache exceeds warning threshold",
				"agent_participant_id", id, "size_bytes", usage.Bytes,
				"threshold_bytes", monitor.thresholdBytes)
		}
	}
	if complete {
		monitor.lastWarned[id] = usage.Bytes
	} else if len(scan.frames) == 0 {
		// Traversal finished with skips: advance the lower-bound watermark,
		// but never lower it or clear it using an incomplete measurement.
		monitor.lastWarned[id] = max(monitor.lastWarned[id], usage.Bytes)
	}
	return warn
}

func (monitor *ToolCacheMonitor) dropScan(id string) {
	if scan := monitor.scans[id]; scan != nil {
		_ = scan.Close()
		delete(monitor.scans, id)
	}
}

// Close releases all retained directory handles and monitoring state.
func (monitor *ToolCacheMonitor) Close() error {
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	var err error
	for id, scan := range monitor.scans {
		err = errors.Join(err, scan.Close())
		delete(monitor.scans, id)
	}
	clear(monitor.lastWarned)
	return err
}

func (monitor *ToolCacheMonitor) Check(ctx context.Context) (checked int, warned int, err error) {
	if monitor == nil || monitor.lifecycle == nil {
		return 0, 0, fmt.Errorf("%w: nil tool cache monitor", ErrInvalidOptions)
	}
	all, err := monitor.lifecycle.listAssignmentParticipantIDs(ctx)
	if err != nil {
		return 0, 0, err
	}
	monitor.mu.Lock()
	for id := range monitor.scans {
		if index := sort.SearchStrings(all, id); index == len(all) || all[index] != id {
			monitor.dropScan(id)
		}
	}
	for id := range monitor.lastWarned {
		if index := sort.SearchStrings(all, id); index == len(all) || all[index] != id {
			delete(monitor.lastWarned, id)
		}
	}
	start := sort.Search(len(all), func(i int) bool { return all[i] > monitor.scanCursor })
	selected := make([]string, 0, min(len(all), monitor.maxAssignments))
	for i := 0; i < len(all) && i < monitor.maxAssignments; i++ {
		selected = append(selected, all[(start+i)%len(all)])
	}
	if len(selected) > 0 {
		monitor.scanCursor = selected[len(selected)-1]
	}
	monitor.mu.Unlock()
	for _, id := range selected {
		if err := ctx.Err(); err != nil {
			return checked, warned, err
		}
		pollCtx, cancel := context.WithTimeout(ctx, toolCachePerAssignmentTimeout)
		_, didWarn, _ := monitor.Observe(pollCtx, id)
		cancel()
		checked++
		if didWarn {
			warned++
		}
	}
	return checked, warned, nil
}

func (monitor *ToolCacheMonitor) Run(ctx context.Context) error {
	if monitor == nil {
		return fmt.Errorf("%w: nil tool cache monitor", ErrInvalidOptions)
	}
	defer monitor.Close()
	for {
		if _, _, err := monitor.Check(ctx); err != nil && ctx.Err() == nil && monitor.onError != nil {
			monitor.onError(err)
		}
		timer := time.NewTimer(monitor.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (lifecycle *Lifecycle) listAssignmentParticipantIDs(ctx context.Context) ([]string, error) {
	root, _, err := openDirectoryNoFollow(lifecycle.miseRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var ids []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err // Incomplete discovery must not prune active scans.
		}
		names, err := root.Readdirnames(toolCacheDirBatchSize)
		for _, name := range names {
			id, found := strings.CutPrefix(name, "assignment-")
			if !found || !uuidtext.Valid(id) {
				continue
			}
			var stat unix.Stat_t
			if unix.Fstatat(int(root.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW) == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				ids = append(ids, id)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(ids)
	return ids, nil
}
