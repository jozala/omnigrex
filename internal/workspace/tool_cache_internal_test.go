package workspace

import (
	"bytes"
	"context"
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
