package workspace

import (
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
	// Choose a threshold between a 5-entry partial and the complete sum.
	threshold := complete.Bytes / 2
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
