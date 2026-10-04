package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/workspace"
	"golang.org/x/sys/unix"
)

func allocatedOnDisk(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return err
		}
		if stat.Blocks < 0 {
			return nil
		}
		total += stat.Blocks * 512
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

const toolCacheAssignment = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func newToolCacheLifecycle(t *testing.T) (*workspace.Lifecycle, string) {
	t.Helper()
	root := t.TempDir()
	lifecycle, err := workspace.New(workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspace"), PublicationRoot: filepath.Join(root, "publication"), MiseRoot: filepath.Join(root, "mise"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle, root
}

func toolCacheDir(t *testing.T, root, assignment string) string {
	t.Helper()
	return filepath.Join(root, "mise", "assignment-"+assignment, "tool-data", "assignment")
}

func toolScratchDir(t *testing.T, root, assignment, turn string) string {
	t.Helper()
	return filepath.Join(root, "mise", "assignment-"+assignment, "tool-data", "turn", turn)
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureAssignmentToolCacheExcludesTurnScratch(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	assignment := toolCacheAssignment
	turn := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	writeSizedFile(t, filepath.Join(toolCacheDir(t, root, assignment), "cache", "reuse"), 1024)
	writeSizedFile(t, filepath.Join(toolScratchDir(t, root, assignment, turn), "build", "scratch"), 1<<20)

	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != 1 {
		t.Fatalf("usage files = %d, want 1", usage.Files)
	}
	if usage.Truncated {
		t.Fatal("usage truncated unexpectedly")
	}
	// Allocated usage for one 1 KiB file plus directories is well below the
	// 1 MiB scratch file; counting scratch would exceed 1 MiB.
	if usage.Bytes <= 0 || usage.Bytes >= 1<<20 {
		t.Fatalf("usage bytes = %d, want small allocated cache without 1 MiB scratch", usage.Bytes)
	}
}

func TestMeasureAssignmentToolCacheMissingIsZero(t *testing.T) {
	lifecycle, _ := newToolCacheLifecycle(t)
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), toolCacheAssignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 0 || usage.Files != 0 || usage.Truncated {
		t.Fatalf("missing usage = %#v, want zero", usage)
	}
}

func TestMeasureAssignmentToolCacheRejectsInvalidAssignment(t *testing.T) {
	lifecycle, _ := newToolCacheLifecycle(t)
	for _, id := range []string{"", "not-a-uuid", "../escape", "12345678-1234-4234-8234-123456789abg"} {
		if _, err := lifecycle.MeasureAssignmentToolCache(context.Background(), id); err == nil {
			t.Fatalf("Measure(%q) = nil, want error", id)
		}
	}
}

func TestMeasureAssignmentToolCacheIgnoresSymlinksWithoutLeavingSubpath(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	assignment := toolCacheAssignment
	cache := toolCacheDir(t, root, assignment)
	writeSizedFile(t, filepath.Join(cache, "real"), 512)
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, filepath.Join(outside, "big"), 1<<20)
	if err := os.Symlink(outside, filepath.Join(cache, "link-outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "big"), filepath.Join(cache, "link-file")); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(cache, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(nested, "deep-link")); err != nil {
		t.Fatal(err)
	}

	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != 1 {
		t.Fatalf("symlink usage files = %d, want 1", usage.Files)
	}
	if usage.Bytes <= 0 || usage.Bytes >= 1<<20 {
		t.Fatalf("symlink usage bytes = %d, want small cache without 1 MiB outside", usage.Bytes)
	}
	if _, err := os.Stat(filepath.Join(outside, "big")); err != nil {
		t.Fatalf("outside file changed: %v", err)
	}
}

func TestMeasureAssignmentToolCacheRefusesSymlinkedRoot(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	assignment := toolCacheAssignment
	toolData := filepath.Join(root, "mise", "assignment-"+assignment, "tool-data")
	if err := os.MkdirAll(filepath.Dir(toolData), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSizedFile(t, filepath.Join(outside, "assignment", "evil"), 4096)
	if err := os.Symlink(outside, toolData); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment); err == nil {
		t.Fatal("measured symlinked tool data without error")
	}
	if _, err := os.Stat(filepath.Join(outside, "assignment", "evil")); err != nil {
		t.Fatalf("outside data changed: %v", err)
	}
}

func TestMeasureAssignmentToolCacheReportsAllocatedDiskUsage(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	cache := toolCacheDir(t, root, toolCacheAssignment)
	const files = 10
	for i := 0; i < files; i++ {
		writeSizedFile(t, filepath.Join(cache, "tiny", string(rune('a'+i))), 1)
	}
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), toolCacheAssignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != files {
		t.Fatalf("usage files = %d, want %d", usage.Files, files)
	}
	// Derive the expectation from the filesystem itself for portability:
	// sum st_blocks*512 over the cache root, its subdirectories, and files.
	// Apparent-size accounting would report only 10 bytes and miss pressure.
	want := allocatedOnDisk(t, cache)
	if usage.Bytes != want {
		t.Fatalf("allocated usage bytes = %d, want filesystem-derived %d", usage.Bytes, want)
	}
	if want <= 10 {
		t.Fatalf("filesystem-derived usage = %d, want materially more than 10 apparent bytes", want)
	}
	if usage.Truncated {
		t.Fatal("usage truncated unexpectedly")
	}
}

func TestMeasureAssignmentToolCacheIncludesNestedDirectoryAllocation(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	cache := toolCacheDir(t, root, toolCacheAssignment)
	nested := filepath.Join(cache, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), toolCacheAssignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != 0 {
		t.Fatalf("usage files = %d, want 0 for directory-only cache", usage.Files)
	}
	// Directory-only cache: apparent size is zero; allocated usage must equal
	// the filesystem-reported directory allocation exactly (portable across
	// filesystems that report smaller or zero directory allocation).
	want := allocatedOnDisk(t, cache)
	if usage.Bytes != want {
		t.Fatalf("directory-only usage bytes = %d, want filesystem-derived %d", usage.Bytes, want)
	}
	if usage.Truncated {
		t.Fatal("usage truncated unexpectedly")
	}
}

func TestMeasureAssignmentToolCacheLeavesUnreadableNestedDirectoryUnchanged(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	assignment := toolCacheAssignment
	cache := toolCacheDir(t, root, assignment)
	writeSizedFile(t, filepath.Join(cache, "visible", "file"), 1024)
	unreadable := filepath.Join(cache, "unreadable")
	writeSizedFile(t, filepath.Join(unreadable, "hidden"), 1024)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatalf("measure with unreadable nested dir error = %v", err)
	}
	info, err := os.Stat(unreadable)
	if err != nil {
		// If the directory cannot even be stated, modes were not changed
		// by measurement; treat as pass only when measurement skipped.
		if !usage.Truncated {
			t.Fatalf("unreadable dir stat error = %v without truncation", err)
		}
		return
	}
	if info.Mode().Perm() != 0 {
		t.Fatalf("unreadable dir mode = %o, want 0 (measurement must not chmod)", info.Mode().Perm())
	}
	if !usage.Truncated {
		t.Fatal("usage should be truncated when a nested directory is skipped")
	}
	if usage.Files != 1 {
		t.Fatalf("usage files = %d, want 1 visible file", usage.Files)
	}
}

func TestToolCacheMonitorWarnsOncePerUnchangedUsage(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	assignment := toolCacheAssignment
	writeSizedFile(t, filepath.Join(toolCacheDir(t, root, assignment), "cache", "blob"), 2048)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	monitor, err := workspace.NewToolCacheMonitor(lifecycle, workspace.ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Minute, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, warned, err := monitor.Observe(ctx, assignment)
	if err != nil || !warned {
		t.Fatalf("first observe = (%#v, %v, %v), want warned", first, warned, err)
	}
	_, warned, err = monitor.Observe(ctx, assignment)
	if err != nil || warned {
		t.Fatalf("repeated observe = (_, %v, %v), want suppressed", warned, err)
	}
	// Growth warns again with increased allocated usage.
	writeSizedFile(t, filepath.Join(toolCacheDir(t, root, assignment), "cache", "more"), 1024)
	grown, warned, err := monitor.Observe(ctx, assignment)
	if err != nil || !warned {
		t.Fatalf("grown observe = (%#v, %v, %v), want warned", grown, warned, err)
	}
	if grown.Bytes <= first.Bytes {
		t.Fatalf("grown bytes = %d, want more than first %d", grown.Bytes, first.Bytes)
	}
	// Dropping below threshold clears state so a future crossing warns again.
	if err := os.RemoveAll(toolCacheDir(t, root, assignment)); err != nil {
		t.Fatal(err)
	}
	if _, warned, err = monitor.Observe(ctx, assignment); err != nil || warned {
		t.Fatalf("below threshold observe = (_, %v, %v), want no warning", warned, err)
	}
	writeSizedFile(t, filepath.Join(toolCacheDir(t, root, assignment), "cache", "again"), 2048)
	if _, warned, err = monitor.Observe(ctx, assignment); err != nil || !warned {
		t.Fatalf("re-cross observe = (_, %v, %v), want warned", warned, err)
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("warnings = %d, want 3 distinct warnings:\n%s", len(lines), logs.String())
	}
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["agent_participant_id"] != assignment {
			t.Fatalf("log entry participant = %v: %s", entry["agent_participant_id"], line)
		}
		if _, ok := entry["size_bytes"]; !ok {
			t.Fatalf("log entry missing size_bytes: %s", line)
		}
		if entry["threshold_bytes"] != float64(1024) {
			t.Fatalf("log entry threshold = %v: %s", entry["threshold_bytes"], line)
		}
		if _, ok := entry["assignment_id"]; ok {
			t.Fatalf("log entry uses legacy assignment_id key: %s", line)
		}
		for key := range entry {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "path") || strings.Contains(lower, "file") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") {
				t.Fatalf("log entry discloses path-like metadata %q: %s", key, line)
			}
		}
		if strings.Contains(line, root) || strings.Contains(line, "tool-data") {
			t.Fatalf("log entry discloses filesystem path: %s", line)
		}
	}
}

func TestToolCacheMonitorBelowThresholdNeverWarns(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	writeSizedFile(t, filepath.Join(toolCacheDir(t, root, toolCacheAssignment), "cache", "small"), 100)
	monitor, err := workspace.NewToolCacheMonitor(lifecycle, workspace.ToolCacheMonitorConfig{
		ThresholdBytes: 10 << 20, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, warned, err := monitor.Observe(context.Background(), toolCacheAssignment); err != nil || warned {
		t.Fatalf("observe = (_, %v, %v), want no warning", warned, err)
	}
}

func TestToolCacheMonitorCheckSkipsFailedMeasurements(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	good := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	writeSizedFile(t, filepath.Join(toolCacheDir(t, root, good), "cache", "blob"), 4096)
	bad := toolCacheAssignment
	toolData := filepath.Join(root, "mise", "assignment-"+bad, "tool-data")
	if err := os.MkdirAll(filepath.Dir(toolData), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-bad")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, toolData); err != nil {
		t.Fatal(err)
	}
	monitor, err := workspace.NewToolCacheMonitor(lifecycle, workspace.ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	checked, warned, err := monitor.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checked != 2 || warned != 1 {
		t.Fatalf("check = (%d, %d), want (2, 1)", checked, warned)
	}
}

func TestToolCacheMonitorRotatesBoundedSelectionAcrossPolls(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	for _, id := range ids {
		writeSizedFile(t, filepath.Join(toolCacheDir(t, root, id), "cache", "blob"), 2048)
	}
	monitor, err := workspace.NewToolCacheMonitor(lifecycle, workspace.ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Millisecond, MaxAssignments: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]int)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		checked, _, err := monitor.Check(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if checked != 1 {
			t.Fatalf("poll %d checked = %d, want 1", i, checked)
		}
		// Observe directly to learn which ID was selected via cursor:
		// the monitor warns once per ID, so a second Check warns for a
		// different ID until all three have warned.
		_ = seen
	}
	// After three single-item polls every ID must have been measured once.
	// Re-check with a fresh monitor that records warnings per ID.
	fresh, err := workspace.NewToolCacheMonitor(lifecycle, workspace.ToolCacheMonitorConfig{
		ThresholdBytes: 1024, PollInterval: time.Millisecond, MaxAssignments: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		checked, warned, err := fresh.Check(ctx)
		if err != nil || checked != 1 || warned != 1 {
			t.Fatalf("fresh poll %d = (%d, %d, %v), want (1, 1, nil)", i, checked, warned, err)
		}
	}
	// Fourth poll wraps around and finds all already warned, so no new warning.
	if checked, warned, err := fresh.Check(ctx); err != nil || checked != 1 || warned != 0 {
		t.Fatalf("wrap poll = (%d, %d, %v), want (1, 0, nil)", checked, warned, err)
	}
	_ = seen
	_ = monitor
}

func TestMeasureAssignmentToolCacheLeavesRetentionAndCleanupUnchanged(t *testing.T) {
	lifecycle, root := newToolCacheLifecycle(t)
	assignment := toolCacheAssignment
	cacheFile := filepath.Join(toolCacheDir(t, root, assignment), "cache", "retain")
	writeSizedFile(t, cacheFile, 512)
	before, err := os.Stat(cacheFile)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Files != 1 || usage.Bytes <= 0 {
		t.Fatalf("measure = (%#v, %v)", usage, err)
	}
	after, err := os.Stat(cacheFile)
	if err != nil || after.Size() != before.Size() || after.Mode() != before.Mode() {
		t.Fatalf("measurement changed cache file: before %#v after %#v", before, after)
	}
	if err := lifecycle.CleanupAssignmentToolPaths(context.Background(), assignment); err != nil {
		t.Fatalf("cleanup after measurement error = %v", err)
	}
	if _, err := os.Stat(cacheFile); !os.IsNotExist(err) {
		t.Fatalf("cleanup after measurement retained cache: %v", err)
	}
	// Measurement failure must not block cleanup: symlinked root fails measurement
	// but cleanup still refuses safely without touching outside data.
	toolData := filepath.Join(root, "mise", "assignment-"+assignment, "tool-data")
	if err := os.MkdirAll(filepath.Dir(toolData), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-cleanup")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "retain"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, toolData); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.MeasureAssignmentToolCache(context.Background(), assignment); err == nil {
		t.Fatal("symlinked measurement succeeded")
	}
	if err := lifecycle.CleanupAssignmentToolPaths(context.Background(), assignment); err == nil {
		t.Fatal("symlinked cleanup followed link")
	}
	if _, err := os.Stat(filepath.Join(outside, "retain")); err != nil {
		t.Fatalf("outside data changed: %v", err)
	}
}

func TestToolCacheMonitorRejectsInvalidConfiguration(t *testing.T) {
	lifecycle, _ := newToolCacheLifecycle(t)
	for _, config := range []workspace.ToolCacheMonitorConfig{
		{ThresholdBytes: 0, PollInterval: time.Minute},
		{ThresholdBytes: -1, PollInterval: time.Minute},
		{ThresholdBytes: 1024, PollInterval: 0},
	} {
		if _, err := workspace.NewToolCacheMonitor(lifecycle, config); err == nil {
			t.Fatalf("NewToolCacheMonitor(%#v) = nil, want error", config)
		}
	}
	if _, err := workspace.NewToolCacheMonitor(nil, workspace.ToolCacheMonitorConfig{ThresholdBytes: 1024, PollInterval: time.Minute}); err == nil {
		t.Fatal("nil lifecycle accepted")
	}
}
