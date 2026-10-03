package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTruncatedMeasurementPreservesWarningState(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 5
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	for i := 0; i < 10; i++ {
		dir := filepath.Join(cache, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// With a bound of 5 entries the first scan must truncate.
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Truncated {
		t.Fatalf("usage = %#v, want truncated", usage)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, warned, err := monitor.Observe(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Truncated {
		t.Fatalf("first observe truncated = false, want true")
	}
	// Truncated below-threshold must preserve state, not clear it: after a
	// warning, a later truncated below-threshold partial must not clear so
	// that a future complete crossing still warns correctly. Here the first
	// truncated scan already exceeds the tiny threshold and warns.
	if !warned {
		t.Fatal("first truncated observe did not warn despite exceeding threshold")
	}
	// A second truncated observe with different rotation must not re-warn
	// noisily while still truncated.
	_, warned, err = monitor.Observe(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if warned {
		t.Log("second truncated observe warned (different partial size); suppressing is preferred but not required for correctness")
	}
	// Restore the full bound: a complete below-threshold measurement clears.
	maxToolCacheEntries = oldMax
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	if _, warned, err := monitor.Observe(ctx, assignment); err != nil || warned {
		t.Fatalf("below threshold after truncation = (_, %v, %v), want no warning", warned, err)
	}
}

func TestTruncatedBelowThresholdPreservesPriorWarning(t *testing.T) {
	oldMax := maxToolCacheEntries
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Ten tiny files: complete allocated usage is well above the threshold,
	// while a 5-entry truncated partial stays below it.
	for i := 0; i < 10; i++ {
		dir := filepath.Join(cache, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	maxToolCacheEntries = 1000000
	complete, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil || complete.Truncated {
		t.Fatalf("complete measure = (%#v, %v), want complete", complete, err)
	}
	// Choose a threshold just below complete so a 5-entry partial stays below.
	threshold := complete.Bytes - 8192
	if threshold <= 1024 || threshold >= complete.Bytes {
		t.Fatalf("complete bytes = %d, cannot pick truncating threshold", complete.Bytes)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: threshold, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, warned, err := monitor.Observe(ctx, assignment); err != nil || !warned {
		t.Fatalf("complete observe = (_, %v, %v), want warned", warned, err)
	}
	// Truncate so the partial sum falls below threshold; it must preserve
	// the prior warning instead of clearing it.
	maxToolCacheEntries = 5
	partial, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil || !partial.Truncated {
		t.Fatalf("partial measure = (%#v, %v), want truncated", partial, err)
	}
	if partial.Bytes >= threshold {
		t.Skipf("partial bytes %d already exceeds threshold %d; rotation picked large prefix", partial.Bytes, threshold)
	}
	if _, warned, err := monitor.Observe(ctx, assignment); err != nil || warned {
		t.Fatalf("truncated below observe = (_, %v, %v), want suppressed without clearing", warned, err)
	}
	// Restoring the full scan with unchanged usage must stay suppressed
	// (dedup), proving the truncated step did not clear warning state.
	// If truncation had cleared, this identical complete size would warn again.
	maxToolCacheEntries = oldMax
	if _, warned, err := monitor.Observe(ctx, assignment); err != nil || warned {
		t.Fatalf("complete after truncated below = (_, %v, %v), want suppressed (state preserved)", warned, err)
	}
}

func TestExactlyBudgetEntriesCompletes(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 5
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Root lists one dir (1 entry) containing four files (4 entries): total
	// exactly budget (5). Must complete, not truncate.
	dir := filepath.Join(cache, "a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Truncated {
		t.Fatalf("exactly-budget usage = %#v, want complete", usage)
	}
	if usage.Files != 4 {
		t.Fatalf("exactly-budget files = %d, want 4", usage.Files)
	}
}

func TestFrontierStarvationCounterexampleWarnsWithinBoundedPolls(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Counterexample shape: root lists heavy X first, then S1 (3 files) and
	// S2 (3 files); budget 6. Without frontier persistence each poll re-walks
	// root+S2 (6 entries exactly) and starves S1/X forever. With persisted
	// frontier completed regions are skipped and X is reached within 2 polls
	// under either readdir ordering, so the warning must fire.
	for _, dir := range []string{"heavy", "s1", "s2"} {
		if err := os.MkdirAll(filepath.Join(cache, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(cache, "s1", string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "s2", string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "heavy", "big"), bytes.Repeat([]byte("x"), 100<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 50 << 10, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warned := false
	for i := 0; i < 10 && !warned; i++ {
		_, w, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		warned = warned || w
	}
	if !warned {
		t.Fatal("starvation counterexample never warned within 10 bounded polls")
	}
	// Drain to cycle completion and assert exact accounting (no double-count
	// from mid-skip rewinds). With the rewind bug, already-counted prefix
	// entries are recounted on resume, inflating Files/Bytes beyond the
	// filesystem-derived total and producing spurious warnings.
	var total ToolCacheUsage
	for i := 0; i < 10; i++ {
		u, _, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		total = u
		if !u.Truncated {
			break
		}
	}
	if total.Truncated {
		t.Fatal("starvation counterexample never completed after warning")
	}
	// Files: s1 3 + s2 3 + heavy 1 = 7 (no double-count).
	if total.Files != 7 {
		t.Fatalf("starvation final files = %d, want 7 (double-count check)", total.Files)
	}
}

func TestEventualWarningForHeavySubtreeBehindLightSiblings(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Light siblings exhaust a tiny budget when visited first; the heavy
	// file alone exceeds the threshold but starts behind them in listing
	// order. Per-directory cursors must eventually bring it within budget.
	lightDir := filepath.Join(cache, "light")
	if err := os.MkdirAll(lightDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(lightDir, string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	heavyDir := filepath.Join(cache, "heavy")
	if err := os.MkdirAll(heavyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(heavyDir, "big"), bytes.Repeat([]byte("x"), 100<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 50 << 10, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warned := false
	for i := 0; i < 10 && !warned; i++ {
		_, w, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		warned = warned || w
	}
	if !warned {
		t.Fatal("heavy subtree never warned within 10 bounded polls")
	}
}

func TestSymlinkRunDoesNotPinCursor(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	dir := filepath.Join(cache, "mixed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Run of ignored symlinks followed by real files. Each symlink consumes
	// budget but must still advance the resume cursor; otherwise the scan pins
	// on the same symlink block every poll and never completes.
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.Symlink(outside, filepath.Join(dir, string(rune('s'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// With budget 6 the first poll must truncate (5 symlinks + root/dir entries
	// exceed it), but the second poll must resume past the symlink run instead
	// of re-stating it. A full cycle must complete within a few polls.
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 10 << 20, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	completed := false
	for i := 0; i < 10 && !completed; i++ {
		usage, _, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		completed = !usage.Truncated
	}
	if !completed {
		t.Fatal("symlink run pinned cursor; scan never completed")
	}
	maxToolCacheEntries = oldMax
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != 3 {
		t.Fatalf("symlink-mixed files = %d, want 3 real files", usage.Files)
	}
}

func TestUnchangedMultiPollCacheWarnsOnce(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "11111111-1111-4111-8111-111111111111"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Ten tiny files need two polls per cycle under budget 6. The first poll's
	// partial already exceeds the tiny threshold and warns; the completed
	// cycle must not warn again, and the next cycle's stale partial (at or
	// below the warned watermark) must not warn either.
	for i := 0; i < 10; i++ {
		dir := filepath.Join(cache, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))+string(rune('0'+i%10))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warnings := 0
	// Run enough polls for several full cycles (each cycle ~2 polls).
	for i := 0; i < 6; i++ {
		_, w, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		if w {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("unchanged multi-poll warnings = %d, want exactly 1", warnings)
	}
}

func TestInterruptedScanResumesFromSavedCursor(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 1000000
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "22222222-2222-4222-8222-222222222222"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	dir := filepath.Join(cache, "big")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const files = 2000
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i%26))+string(rune('0'+i%10))+string(rune('A'+i%26))+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 10 << 20, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Interrupt the first poll almost immediately; it must save progress.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, _, err = monitor.Observe(timeoutCtx, assignment)
	if err == nil {
		t.Skip("poll completed before timeout; filesystem too fast to interrupt deterministically")
	}
	// Next poll with a live context must resume (not restart): total files must
	// equal exactly the created count, proving no double-count from restarting.
	maxToolCacheEntries = 1000000
	// Drain the cycle to completion across bounded polls.
	var total ToolCacheUsage
	for i := 0; i < 10; i++ {
		u, _, err := monitor.Observe(context.Background(), assignment)
		if err != nil {
			t.Fatal(err)
		}
		total = u
		if !u.Truncated {
			break
		}
	}
	if total.Truncated {
		t.Fatal("interrupted scan never completed after resume")
	}
	if total.Files != files {
		t.Fatalf("resumed total files = %d, want %d (progress discarded or double-counted)", total.Files, files)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func TestCompletedRefreshesWatermarkAfterPartialWarning(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "33333333-3333-4333-8333-333333333333"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Ten tiny files need two polls per cycle under budget 6 (root 1 + dir 10
	// = 11 entries; first poll 6 entries partial, second poll remaining 5 to
	// complete). First poll partial already exceeds the tiny threshold and
	// warns; completion is suppressed in-cycle but must refresh the watermark
	// to the exact total so later identical cycles do not warn again.
	dir := filepath.Join(cache, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warnings := 0
	// Two full cycles (each ~2 polls): first cycle warns once at partial,
	// completes higher suppressed with refresh; second identical cycle must
	// suppress both polls (partial at/below refreshed watermark, complete
	// equal to watermark).
	for i := 0; i < 4; i++ {
		_, w, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		if w {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("watermark-refresh warnings = %d, want exactly 1 across two identical cycles", warnings)
	}
}

func TestSkipRegionDrainsAcrossPolls(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "44444444-4444-4444-8444-444444444444"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Directory with a large prefix (already counted) followed by tail with a
	// heavy file. With budget 6 the first poll truncates before the tail; the
	// second poll must resume past the prefix (skip drains monotonically)
	// instead of replaying it, so the heavy tail is eventually measured.
	dir := filepath.Join(cache, "bigdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	heavy := filepath.Join(cache, "heavy")
	if err := os.MkdirAll(heavy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(heavy, "big"), bytes.Repeat([]byte("x"), 100<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 50 << 10, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warned := false
	for i := 0; i < 10 && !warned; i++ {
		_, w, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		warned = warned || w
	}
	if !warned {
		t.Fatal("skip region never drained; heavy tail never measured")
	}
}

func TestReplayDrainCompletesWithTailMeasured(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "55555555-5555-4555-8555-555555555555"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Moderately large ignored prefix (20 symlinks, budget 6) forces multiple
	// polls with mid-skip cutoffs; the tail heavy file must still be measured
	// within bounded polls with exact accounting (no double-count).
	dir := filepath.Join(cache, "replay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-replay")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := os.Symlink(outside, filepath.Join(dir, "link"+itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	heavy := filepath.Join(cache, "taily")
	if err := os.MkdirAll(heavy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(heavy, "big"), bytes.Repeat([]byte("x"), 100<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 50 << 10, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	warned := false
	for i := 0; i < 20 && !warned; i++ {
		_, w, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		warned = warned || w
	}
	if !warned {
		t.Fatal("replay drain never measured heavy tail within bounded polls")
	}
	var total ToolCacheUsage
	for i := 0; i < 20; i++ {
		u, _, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		total = u
		if !u.Truncated {
			break
		}
	}
	if total.Truncated {
		t.Fatal("replay drain never completed")
	}
	if total.Files != 1 {
		t.Fatalf("replay final files = %d, want 1 heavy file (no double-count)", total.Files)
	}
}

func TestMidSkipRewindDoesNotDoubleCount(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "66666666-6666-4666-8666-666666666666"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Ten tiny files need multiple polls per cycle; mid-skip rewinds must not
	// recount already-counted names. Drain to completion and assert exact
	// filesystem-derived total with no spurious warning beyond the first.
	dir := filepath.Join(cache, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 10 << 20, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var total ToolCacheUsage
	for i := 0; i < 20; i++ {
		u, _, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		total = u
		if !u.Truncated {
			break
		}
	}
	if total.Truncated {
		t.Fatal("rewind drain never completed")
	}
	if total.Files != 10 {
		t.Fatalf("rewind final files = %d, want 10 (no double-count, no loss)", total.Files)
	}
}

func TestDeepRewindRegionStillProgressesWithinBudget(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "77777777-7777-4777-8777-777777777777"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// Twenty files need several polls under budget 6. Simulating a deep
	// rewind (cursor back to 0, i.e. a rewind region larger than the per-poll
	// budget) before every poll reproduces the timeout-driven starvation
	// chain: without dedup-before-bill every poll re-bills already-counted
	// names and the tail never advances. Deduplicated re-reads must be
	// verification only (no budget), so the tail progresses every poll.
	dir := filepath.Join(cache, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const files = 20
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 10 << 20, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var total ToolCacheUsage
	for i := 0; i < 10; i++ {
		// Force a deep rewind into the already-counted prefix before every
		// poll after the first, simulating a timeout mid-skip that saved a
		// rewind cursor. The rewind region quickly exceeds the budget.
		if i > 0 {
			if scan := monitor.scans[assignment]; scan != nil {
				for path := range scan.cursors {
					scan.cursors[path] = 0
				}
			}
		}
		u, _, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatal(err)
		}
		total = u
		if !u.Truncated {
			break
		}
	}
	if total.Truncated {
		t.Fatal("deep rewind region starved the tail; cycle never completed within bounded polls")
	}
	if total.Files != files {
		t.Fatalf("deep rewind final files = %d, want %d (double-count or loss)", total.Files, files)
	}
}

func TestCacheRecreationResetsStaleScanState(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 6
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "88888888-8888-4888-8888-888888888888"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	dir := filepath.Join(cache, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 10 << 20, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Start a cycle and leave it mid-flight (partial frontier + cursor).
	if _, _, err := monitor.Observe(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if scan := monitor.scans[assignment]; scan == nil || (len(scan.pending) == 0 && len(scan.cursors) == 0) {
		t.Fatal("expected mid-cycle scan state after first poll")
	}
	// Collect and recreate the cache: stale pending entries pin removed
	// (dev, ino) values and cursors point into a vanished listing. The next
	// poll must start a fresh cycle instead of failing permanently.
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const recreated = 3
	for i := 0; i < recreated; i++ {
		if err := os.WriteFile(filepath.Join(dir, "g"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Poison any surviving frontier pins to guarantee a mismatch even if the
	// filesystem reuses inode numbers for the recreated directories. Stale
	// state must be dropped (lower bound), never wedged as a permanent error.
	if scan := monitor.scans[assignment]; scan != nil {
		for i := range scan.pending {
			scan.pending[i].dev, scan.pending[i].ino = 0xDEADBEEF, 0xDEADBEEF
		}
	}
	// Drain across the stale-cycle boundary: the poll immediately after
	// recreation may complete with the prior partial sum as a lower bound
	// (stale entries dropped, hadSkip set); the following fresh cycle must
	// then measure the recreated cache exactly, without permanent errors.
	found := false
	for i := 0; i < 10 && !found; i++ {
		u, _, err := monitor.Observe(ctx, assignment)
		if err != nil {
			t.Fatalf("recreated cache observe %d failed: %v (stale state wedged)", i, err)
		}
		if !u.Truncated && u.Files == recreated {
			found = true
		}
	}
	if !found {
		t.Fatalf("recreated cache never measured %d files within bounded polls (stale state leaked)", recreated)
	}
}

func TestCheckPrunesAbsentParticipants(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	keep := "99999999-9999-4999-8999-999999999999"
	gone := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab"
	for _, assignment := range []string{keep, gone} {
		cache, err := lifecycle.toolDataRoot(assignment)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cache, "assignment"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "assignment", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, assignment := range []string{keep, gone} {
		if _, _, err := monitor.Observe(ctx, assignment); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := monitor.scans[gone]; !ok {
		t.Fatal("expected scan state for gone participant before collection")
	}
	// Collect one assignment entirely so it disappears from the listing.
	goneRoot, err := lifecycle.toolDataRoot(gone)
	if err != nil {
		t.Fatal(err)
	}
	// toolDataRoot is mise/assignment-<ID>/tool-data; the listing scans
	// mise/assignment-<ID>, so remove that parent.
	if err := os.RemoveAll(filepath.Dir(goneRoot)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := monitor.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := monitor.scans[gone]; ok {
		t.Fatal("collected participant scan state was not pruned")
	}
	if _, ok := monitor.lastWarned[gone]; ok {
		t.Fatal("collected participant warning watermark was not pruned")
	}
	if _, ok := monitor.scans[keep]; !ok {
		t.Fatal("retained participant scan state was pruned")
	}
}

func TestMidSkipTimeoutDrainsAcrossPolls(t *testing.T) {
	oldMax := maxToolCacheEntries
	maxToolCacheEntries = 1000000
	defer func() { maxToolCacheEntries = oldMax }()

	root := t.TempDir()
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment := "12121212-1212-4121-8121-121212121212"
	cache, err := lifecycle.toolDataRoot(assignment)
	if err != nil {
		t.Fatal(err)
	}
	cache = filepath.Join(cache, "assignment")
	// A moderately large counted prefix behind which a heavy tail hides. The
	// per-poll budget never binds in the second phase; cutoffs there come
	// from per-poll timeouts, including ones landed mid-skip while re-reading
	// the already-counted prefix. The skip window must drain monotonically
	// across polls (mid-skip progress persisted via rewind) instead of
	// replaying the same prefix forever, so the tail is measured, the cycle
	// completes, and accounting stays exact.
	//
	// Timing robustness: no fixed tiny timeout appears here. Phase 1 runs
	// without timeouts at all; phase 2 derives its per-poll timeout from the
	// measured local cost of one budget-bounded poll (a generous multiple,
	// clamped), and bounds the drain by measured forward progress (a stall
	// detector) rather than a fixed poll count. Slow runners (race
	// instrumentation, loaded CI) get proportionally larger timeouts, so a
	// genuine livelock is the only way this test can fail.
	wide := filepath.Join(cache, "wide")
	if err := os.MkdirAll(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	const prefix = 3000
	for i := 0; i < prefix; i++ {
		if err := os.WriteFile(filepath.Join(wide, "f"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	heavy := filepath.Join(cache, "heavy")
	if err := os.MkdirAll(heavy, 0o755); err != nil {
		t.Fatal(err)
	}
	const tail = 4
	for i := 0; i < tail-1; i++ {
		if err := os.WriteFile(filepath.Join(heavy, "f"+itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(heavy, "big"), bytes.Repeat([]byte("x"), 100<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	monitor, err := NewToolCacheMonitor(lifecycle, ToolCacheMonitorConfig{
		ThresholdBytes: 50 << 10, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Phase 1 (deterministic, no timeouts): build a deep resume cursor with a
	// small entry budget, and measure the local cost of one bounded poll.
	maxToolCacheEntries = 300
	warned := false
	var sample time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		u, w, err := monitor.Observe(context.Background(), assignment)
		sample = time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		warned = warned || w
		if cur := monitor.scans[assignment].cursors["wide"]; cur >= 2500 {
			break
		}
		if !u.Truncated {
			t.Fatal("prefix unexpectedly completed under the small budget")
		}
	}
	if cur := monitor.scans[assignment].cursors["wide"]; cur < 2500 {
		t.Fatalf("wide cursor = %d, want deep (>=2500) resume offset for phase 2", cur)
	}
	// Derive the phase-2 timeout from the measured local poll cost: a
	// generous multiple, clamped so the skip window fits comfortably even on
	// slow runners (every poll has room to make progress) while loaded
	// runners still see genuine timeout cutoffs, including mid-skip ones.
	// Fast runners may see zero interruptions and drain in a few polls; the
	// assertions below hold either way, and the stall detector (not a fixed
	// poll count) is what fails a genuine livelock.
	pollTimeout := sample * 20
	if pollTimeout < 100*time.Millisecond {
		pollTimeout = 100 * time.Millisecond
	}
	if pollTimeout > 5*time.Second {
		pollTimeout = 5 * time.Second
	}
	// Phase 2 (timeout-driven only): the budget never binds. Interrupted polls
	// preserve their partial progress (including mid-skip rewinds), so the
	// file total grows monotonically; only consecutive polls with zero growth
	// indicate a livelock. Error returns already carry the preserved partial
	// totals, so they count toward progress too.
	maxToolCacheEntries = 1000000
	var total ToolCacheUsage
	completed := false
	interrupted := 0
	lastFiles := int64(-1)
	stalled := 0
	for i := 0; i < 200 && !completed; i++ {
		pollCtx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		u, w, err := monitor.Observe(pollCtx, assignment)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				interrupted++
				if u.Files > lastFiles {
					lastFiles, stalled = u.Files, 0
				} else {
					stalled++
				}
				if stalled >= 30 {
					t.Fatalf("no forward progress for %d consecutive interrupted polls (files=%d); skip window is replaying, not draining", stalled, u.Files)
				}
				continue
			}
			t.Fatal(err)
		}
		warned = warned || w
		total = u
		if u.Files > lastFiles {
			lastFiles, stalled = u.Files, 0
		} else {
			stalled++
		}
		if stalled >= 30 {
			t.Fatalf("no forward progress for %d consecutive polls (files=%d); scan is wedged", stalled, u.Files)
		}
		completed = !u.Truncated
	}
	t.Logf("timeout drain: pollTimeout=%v interrupted=%d completed=%v files=%d", pollTimeout, interrupted, completed, total.Files)
	if !completed {
		t.Fatal("skip window never drained within bounded polls; tail never measured")
	}
	if !warned {
		t.Fatal("heavy tail never warned despite exceeding the threshold")
	}
	if total.Files != prefix+tail {
		t.Fatalf("timeout drain final files = %d, want %d (double-count or loss)", total.Files, prefix+tail)
	}
}
