package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const streamingParticipant = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func streamingFixture(t *testing.T, count int) (*Lifecycle, string) {
	t.Helper()
	root := t.TempDir()
	lifecycle, err := New(Options{WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise")})
	if err != nil {
		t.Fatal(err)
	}
	cache := streamingCache(t, lifecycle, streamingParticipant, count)
	return lifecycle, cache
}

func streamingCache(t *testing.T, lifecycle *Lifecycle, id string, count int) string {
	t.Helper()
	root, err := lifecycle.toolDataRoot(id)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "assignment")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if err := os.WriteFile(filepath.Join(cache, fmt.Sprintf("file-%04d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cache
}

func streamingMonitor(t *testing.T, lifecycle *Lifecycle, budget int, threshold int64) *ToolCacheMonitor {
	t.Helper()
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{ThresholdBytes: threshold, PollInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	monitor.entryBudget = budget
	t.Cleanup(func() { _ = monitor.Close() })
	return monitor
}

func streamingAllocated(t *testing.T, root string) int64 {
	t.Helper()
	seen := make(map[cacheFileIdentity]bool)
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink == 0 && !seen[cacheIdentity(stat)] {
			seen[cacheIdentity(stat)] = true
			total += stat.Blocks * 512
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func streamingDrain(t *testing.T, monitor *ToolCacheMonitor, id string) ToolCacheUsage {
	t.Helper()
	for i := 0; i < 2000; i++ {
		usage, _, err := monitor.Observe(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !usage.Truncated {
			return usage
		}
	}
	t.Fatal("scan failed to complete")
	return ToolCacheUsage{}
}

// A deterministic deadline cuts every slice off after the same number of
// cancellation checkpoints, without relying on machine speed or wall time.
type streamingDeadline struct {
	context.Context
	remaining int
}

func (ctx *streamingDeadline) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.DeadlineExceeded
	}
	return nil
}

func TestStreamingRepeatedDeadlinesAdvanceWithoutReplay(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 600)
	monitor := streamingMonitor(t, lifecycle, 384, 1<<30)
	first, _, err := monitor.Observe(context.Background(), streamingParticipant)
	if err != nil || first.Files != 384 || !first.Truncated {
		t.Fatalf("first slice = (%#v, %v)", first, err)
	}
	iterator := monitor.scans[streamingParticipant].frames[0].file
	last := first.Files
	interrupted := 0
	for i := 0; i < 40; i++ {
		ctx := &streamingDeadline{Context: context.Background(), remaining: 20}
		usage, _, err := monitor.Observe(ctx, streamingParticipant)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if !usage.Truncated {
			if interrupted < 2 || usage.Files != 600 || usage.Bytes != streamingAllocated(t, cache) {
				t.Fatalf("completed = %#v after %d interruptions", usage, interrupted)
			}
			if _, err := iterator.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("completed iterator not closed: %v", err)
			}
			return
		}
		interrupted++
		if usage.Files <= last || monitor.scans[streamingParticipant].frames[0].file != iterator {
			t.Fatalf("no iterator-preserving progress: previous=%d current=%#v", last, usage)
		}
		last = usage.Files
	}
	t.Fatal("repeated deadlines stranded the tail")
}

func TestStreamingBudgetsPreserveBufferedSiblingsAndIgnoredEntries(t *testing.T) {
	for _, budget := range []int{1, 5, 128, 129} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			lifecycle, cache := streamingFixture(t, 300)
			if err := os.Symlink("/outside", filepath.Join(cache, "ignored")); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				path := filepath.Join(cache, fmt.Sprintf("child-%d", i))
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "blob"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			monitor := streamingMonitor(t, lifecycle, budget, 1<<30)
			usage := streamingDrain(t, monitor, streamingParticipant)
			if usage.Files != 305 || usage.Bytes != streamingAllocated(t, cache) {
				t.Fatalf("lost/replayed buffered entries: %#v", usage)
			}
		})
	}
}

func TestStreamingExactlyBudgetCompletes(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 10)
	monitor := streamingMonitor(t, lifecycle, 10, 1<<30)
	usage, _, err := monitor.Observe(context.Background(), streamingParticipant)
	if err != nil || usage.Truncated || usage.Files != 10 || usage.Bytes != streamingAllocated(t, cache) {
		t.Fatalf("exact boundary = (%#v, %v)", usage, err)
	}
}

func TestStreamingHardlinksDeduplicateAcrossDirectoriesAndSlices(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 0)
	a, b := filepath.Join(cache, "a"), filepath.Join(cache, "b")
	for _, path := range []string{a, b} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	blob := filepath.Join(a, "blob")
	if err := os.WriteFile(blob, make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(blob, filepath.Join(b, "alias")); err != nil {
		t.Fatal(err)
	}
	actual := streamingAllocated(t, cache)
	monitor := streamingMonitor(t, lifecycle, 1, actual+1)
	var warned bool
	for i := 0; i < 20; i++ {
		usage, didWarn, err := monitor.Observe(context.Background(), streamingParticipant)
		if err != nil {
			t.Fatal(err)
		}
		warned = warned || didWarn
		if !usage.Truncated {
			if warned || usage.Bytes != actual || usage.Files != 1 {
				t.Fatalf("false threshold crossing: actual=%d observed=%#v warned=%v", actual, usage, warned)
			}
			return
		}
	}
	t.Fatal("hardlink scan never completed")
}

func TestStreamingOrdinaryFilesRetainNoIdentitySet(t *testing.T) {
	lifecycle, _ := streamingFixture(t, 1000)
	monitor := streamingMonitor(t, lifecycle, 5, 1<<30)
	for i := 0; i < 10; i++ {
		if _, _, err := monitor.Observe(context.Background(), streamingParticipant); err != nil {
			t.Fatal(err)
		}
		scan := monitor.scans[streamingParticipant]
		if len(scan.hardlinks) != 0 || len(scan.frames) != 1 || len(scan.frames[0].names) > toolCacheDirBatchSize {
			t.Fatalf("ordinary-file state grew: links=%d frames=%d", len(scan.hardlinks), len(scan.frames))
		}
	}
}

func TestStreamingWarningBaselineSurvivesDifferentSliceBoundaries(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 12)
	monitor := streamingMonitor(t, lifecycle, 3, 1024)
	var logs bytes.Buffer
	monitor.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	for _, budget := range []int{3, 7, 2} {
		monitor.entryBudget = budget
		usage := streamingDrain(t, monitor, streamingParticipant)
		if usage.Bytes != monitor.lastWarned[streamingParticipant] || usage.Bytes != streamingAllocated(t, cache) {
			t.Fatal("completed measurement did not refresh suppression baseline")
		}
	}
	if count := strings.Count(logs.String(), "exceeds warning threshold"); count != 1 {
		t.Fatalf("unchanged cache emitted %d warnings", count)
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	if _, warned, err := monitor.Observe(context.Background(), streamingParticipant); err != nil || warned {
		t.Fatalf("missing cache observation = (%v, %v)", warned, err)
	}
	if _, warned := monitor.lastWarned[streamingParticipant]; warned {
		t.Fatal("collected cache warning retained")
	}
}

func TestStreamingIncompleteBelowThresholdPreservesWarning(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 12)
	threshold := streamingAllocated(t, cache) - 1
	monitor := streamingMonitor(t, lifecycle, 100, threshold)
	if _, warned, err := monitor.Observe(context.Background(), streamingParticipant); err != nil || !warned {
		t.Fatalf("complete warning = (%v, %v)", warned, err)
	}
	monitor.entryBudget = 1
	partial, warned, err := monitor.Observe(context.Background(), streamingParticipant)
	if err != nil || warned || !partial.Truncated || partial.Bytes >= threshold {
		t.Fatalf("partial = (%#v, %v, %v)", partial, warned, err)
	}
	if _, present := monitor.lastWarned[streamingParticipant]; !present {
		t.Fatal("partial below threshold cleared prior warning")
	}
}

func TestStreamingCollectionAndRootReplacementCloseHandles(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 20)
	monitor := streamingMonitor(t, lifecycle, 1, 1<<30)
	if _, _, err := monitor.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := monitor.scans[streamingParticipant].frames[0].file
	if err := os.Rename(cache, cache+"-old"); err != nil {
		t.Fatal(err)
	}
	streamingCache(t, lifecycle, streamingParticipant, 2)
	usage := streamingDrain(t, monitor, streamingParticipant)
	if usage.Files != 2 {
		t.Fatalf("recreated cache inherited stale accounting: %#v", usage)
	}
	if _, err := old.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("replaced root iterator retained: %v", err)
	}
	if _, _, err := monitor.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	old = monitor.scans[streamingParticipant].frames[0].file
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(cache))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := monitor.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(monitor.scans) != 0 {
		t.Fatal("collected scan retained")
	}
	if _, err := old.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("collected root iterator retained: %v", err)
	}
}

func TestStreamingMovedSubtreeIsNotTraversed(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 0)
	child := filepath.Join(cache, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	monitor := streamingMonitor(t, lifecycle, 1, 1<<30)
	if _, _, err := monitor.Observe(context.Background(), streamingParticipant); err != nil {
		t.Fatal(err)
	}
	retained := monitor.scans[streamingParticipant].frames[1].file
	if err := os.Rename(child, filepath.Join(filepath.Dir(cache), "outside-cache")); err != nil {
		t.Fatal(err)
	}
	usage, _, err := monitor.Observe(context.Background(), streamingParticipant)
	if err != nil || usage.Files != 0 || !usage.Truncated {
		t.Fatalf("moved subtree measured: (%#v, %v)", usage, err)
	}
	if _, err := retained.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("moved subtree iterator retained: %v", err)
	}
}

func TestStreamingActiveScanLimitAndShutdownReleaseHandles(t *testing.T) {
	lifecycle, _ := streamingFixture(t, 20)
	monitor := streamingMonitor(t, lifecycle, 1, 1<<30)
	var retained []*os.File
	for i := 0; i < maxActiveToolCacheScans+1; i++ {
		id := fmt.Sprintf("%08d-0000-4000-8000-%012d", i, i)
		streamingCache(t, lifecycle, id, 20)
		_, _, err := monitor.Observe(context.Background(), id)
		if i == maxActiveToolCacheScans {
			if !errors.Is(err, errToolCacheScanLimit) {
				t.Fatalf("scan limit error = %v", err)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, monitor.scans[id].frames[0].file)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := monitor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown = %v", err)
	}
	for _, file := range retained {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("shutdown retained an iterator: %v", err)
		}
	}
}

func TestStreamingResourceLimitsNeverReportExactUsage(t *testing.T) {
	for _, resource := range []string{"depth", "hardlinks"} {
		t.Run(resource, func(t *testing.T) {
			lifecycle, cache := streamingFixture(t, 0)
			if resource == "depth" {
				if err := os.MkdirAll(filepath.Join(cache, "a", "b", "c"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				for i := 0; i < 3; i++ {
					file := filepath.Join(cache, fmt.Sprint(i))
					if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(file, file+"-alias"); err != nil {
						t.Fatal(err)
					}
				}
			}
			directory, stat, err := lifecycle.openToolCacheRoot(streamingParticipant)
			if err != nil {
				t.Fatal(err)
			}
			scan := newToolCacheScan(directory, stat)
			defer scan.Close()
			scan.depthLimit, scan.hardlinkLimit = 2, 2
			usage, err := scan.Step(context.Background(), 100)
			if err != nil || !usage.Truncated || len(scan.frames) != 0 || len(scan.hardlinks) > 2 {
				t.Fatalf("unbounded or exact resource-limited scan: usage=%#v err=%v", usage, err)
			}
		})
	}
}

type streamingActionContext struct {
	context.Context
	action func()
}

func (ctx streamingActionContext) Err() error {
	ctx.action()
	return ctx.Context.Err()
}

func TestStreamingSubtreeMovedDuringSliceIsSkipped(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 0)
	child := filepath.Join(cache, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, stat, err := lifecycle.openToolCacheRoot(streamingParticipant)
	if err != nil {
		t.Fatal(err)
	}
	scan := newToolCacheScan(directory, stat)
	defer scan.Close()
	moved := false
	ctx := streamingActionContext{Context: context.Background(), action: func() {
		if !moved && len(scan.frames) > 1 {
			if err := os.Rename(child, filepath.Join(filepath.Dir(cache), "outside-cache")); err != nil {
				t.Fatal(err)
			}
			moved = true
		}
	}}
	usage, err := scan.Step(ctx, 100)
	if err != nil || !moved || usage.Files != 0 || !usage.Truncated {
		t.Fatalf("traversed relocated subtree in same slice: usage=%#v err=%v moved=%v", usage, err, moved)
	}
}

func TestStreamingDeadlineCrossingWarnsAndDeduplicates(t *testing.T) {
	lifecycle, _ := streamingFixture(t, 100)
	monitor := streamingMonitor(t, lifecycle, 100, 1024)
	for i := 0; i < 3; i++ {
		ctx := &streamingDeadline{Context: context.Background(), remaining: 8}
		usage, warned, err := monitor.Observe(ctx, streamingParticipant)
		if !errors.Is(err, context.DeadlineExceeded) || usage.Bytes < monitor.thresholdBytes || warned != (i == 0) {
			t.Fatalf("deadline crossing %d = (%#v, %v, %v)", i, usage, warned, err)
		}
	}
}

func TestStreamingRepeatedTruncatedCyclesStaySuppressed(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 20)
	deep := cache
	for i := 0; i < maxToolCacheDepth+1; i++ {
		deep = filepath.Join(deep, "nested")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	monitor := streamingMonitor(t, lifecycle, 3, 1024)
	warnings := 0
	for _, budget := range []int{3, 7, 2} {
		monitor.entryBudget = budget
		finished := false
		for i := 0; i < 100; i++ {
			usage, warned, err := monitor.Observe(context.Background(), streamingParticipant)
			if err != nil {
				t.Fatal(err)
			}
			if warned {
				warnings++
			}
			if monitor.scans[streamingParticipant] == nil {
				if !usage.Truncated || usage.Files != 20 {
					t.Fatalf("resource-limited cycle = %#v", usage)
				}
				finished = true
				break
			}
		}
		if !finished {
			t.Fatal("resource-limited traversal failed to finish")
		}
	}
	if warnings != 1 {
		t.Fatalf("unchanged truncated cycles emitted %d warnings", warnings)
	}
}

func TestStreamingAdmissionRotatesWithoutStarvingWaitingParticipants(t *testing.T) {
	lifecycle, cache := streamingFixture(t, 0)
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(cache))); err != nil {
		t.Fatal(err)
	}
	monitor := streamingMonitor(t, lifecycle, 2, 1024)
	monitor.maxAssignments = 1
	for i := 0; i < maxActiveToolCacheScans+2; i++ {
		id := fmt.Sprintf("%08d-0000-4000-8000-%012d", i, i)
		streamingCache(t, lifecycle, id, 12)
	}
	for poll := 0; poll < 200; poll++ {
		if _, _, err := monitor.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(monitor.scans) > maxActiveToolCacheScans {
			t.Fatal("active scan limit exceeded")
		}
		if len(monitor.lastWarned) == maxActiveToolCacheScans+2 {
			return
		}
	}
	t.Fatal("waiting Participants never admitted after active scans completed")
}

func TestStreamingFailedDiscoveryDoesNotDiscardIterators(t *testing.T) {
	lifecycle, _ := streamingFixture(t, 20)
	monitor := streamingMonitor(t, lifecycle, 1, 1<<30)
	if _, _, err := monitor.Observe(context.Background(), streamingParticipant); err != nil {
		t.Fatal(err)
	}
	retained := monitor.scans[streamingParticipant].frames[0].file
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := monitor.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("discovery cancellation = %v", err)
	}
	if monitor.scans[streamingParticipant] == nil {
		t.Fatal("incomplete discovery pruned an active Participant")
	}
	if _, err := retained.Stat(); err != nil {
		t.Fatalf("incomplete discovery closed its iterator: %v", err)
	}
}
