package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func elapsed(seconds float64) *float64 { return &seconds }

func writeCapture(t *testing.T, directory, label, events string, complete bool, exitCode int) {
	t.Helper()
	metadata := captureMetadata{Label: label, WallSeconds: elapsed(12), Complete: complete, ExitCode: &exitCode}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{label + ".run.json": string(data), label + ".events.jsonl": events} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReportSeparatesNestedTestsPackagesAndRepeatedFixtureSamples(t *testing.T) {
	directory := t.TempDir()
	var events strings.Builder
	for _, event := range []testEvent{
		{Action: "start", Package: "store"},
		{Action: "run", Package: "store", Test: "TestParent"},
		{Action: "run", Package: "store", Test: "TestParent/child"},
		{Action: "output", Package: "store", Test: "TestParent/child", Output: "    fixture_test.go:42: fixture_timing {\"fixture\":\"1\",\"phase\":\"postgres_start\",\"elapsed_ns\":1000000000,\"outcome\":\"ok\"}\n"},
		{Action: "output", Package: "store", Test: "TestParent/child", Output: "    fixture_test.go:42: fixture_timing {\"fixture\":\"1\",\"phase\":\"store_open\",\"elapsed_ns\":2000000000,\"outcome\":\"ok\"}\n"},
		{Action: "output", Package: "store", Test: "TestParent/child", Output: "    fixture_test.go:42: fixture_timing {\"fixture\":\"1\",\"phase\":\"store_open\",\"elapsed_ns\":4000000000,\"outcome\":\"error\"}\n"},
		{Action: "pass", Package: "store", Test: "TestParent/child", Elapsed: elapsed(8)},
		{Action: "pass", Package: "store", Test: "TestParent", Elapsed: elapsed(10)},
		{Action: "pass", Package: "store", Elapsed: elapsed(11)},
	} {
		data, _ := json.Marshal(event)
		fmt.Fprintln(&events, string(data))
	}
	writeCapture(t, directory, "integration", events.String(), true, 0)
	report := readReport(directory, []string{"integration"})
	if len(report.Runs) != 1 {
		t.Fatalf("runs = %+v", report.Runs)
	}
	run := report.Runs[0]
	if !run.Complete || run.WallSeconds == nil || *run.WallSeconds != 12 || len(run.Tests) != 2 || len(run.Packages) != 1 || run.Fixtures != 1 {
		t.Fatalf("run = %+v", run)
	}
	if len(run.Phases) != 2 || run.Phases[1].Phase != "store_open" || run.Phases[1].Count != 2 || run.Phases[1].TotalSeconds != 6 || run.Phases[1].MedianSeconds != 3 || run.Phases[1].P95Seconds != 4 || run.Phases[1].Errors != 1 {
		t.Fatalf("phases = %+v", run.Phases)
	}
	var markdown bytes.Buffer
	writeMarkdown(&markdown, report)
	text := markdown.String()
	if !strings.Contains(text, "TestParent/child") || !strings.Contains(text, "TestParent") || !strings.Contains(text, "not wall time") || !strings.Contains(text, "store_open includes") {
		t.Fatalf("missing report interpretation: %s", text)
	}
}

func TestReportMarksMissingTruncatedFailedAndSkippedResults(t *testing.T) {
	directory := t.TempDir()
	writeCapture(t, directory, "broken", `{"Action":"start","Package":"store"}
{"Action":"run","Package":"store","Test":"TestInterrupted"}
{"Action":"skip","Package":"other","Test":"TestSkipped","Elapsed":0}
{"Action":"fail","Package":"other","Test":"TestFailed","Elapsed":3}
{"Action":"fail","Package":"other","Elapsed":4}
{"Action":`, false, 1)
	report := readReport(directory, []string{"broken", "missing"})
	if len(report.Runs) != 2 || report.Runs[0].Complete || report.Runs[1].Complete || len(report.Runs[0].Issues) < 2 || len(report.Runs[1].Issues) == 0 {
		t.Fatalf("incomplete reports = %+v", report)
	}
	if report.Runs[0].Failed != 1 || report.Runs[0].Skipped != 1 || len(report.Runs[0].Tests) != 2 || report.Runs[0].ExitCode == nil || *report.Runs[0].ExitCode != 1 {
		t.Fatalf("failed/skipped results = %+v", report.Runs[0])
	}
}

func TestReportHandlesInterleavedAndRepeatedTestExecutions(t *testing.T) {
	directory := t.TempDir()
	writeCapture(t, directory, "unit", `{"Action":"start","Package":"a"}
{"Action":"start","Package":"b"}
{"Action":"run","Package":"a","Test":"TestSame"}
{"Action":"run","Package":"b","Test":"TestSame"}
{"Action":"pass","Package":"a","Test":"TestSame","Elapsed":2}
{"Action":"run","Package":"a","Test":"TestSame"}
{"Action":"pass","Package":"b","Test":"TestSame","Elapsed":3}
{"Action":"pass","Package":"a","Test":"TestSame","Elapsed":4}
{"Action":"pass","Package":"a","Elapsed":6}
{"Action":"pass","Package":"b","Elapsed":3}
`, true, 0)
	run := readReport(directory, []string{"unit"}).Runs[0]
	if !run.Complete || len(run.Tests) != 3 || len(run.Packages) != 2 {
		t.Fatalf("interleaved results = %+v", run)
	}
}

func TestUnknownWallTimeIsNullAndMeasuredWallTimeSurvivesIncompleteEvents(t *testing.T) {
	for _, test := range []struct {
		name string
		wall *float64
	}{
		{"unknown", nil},
		{"zero", elapsed(0)},
		{"measured", elapsed(12)},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			metadata := map[string]any{"label": "interrupted", "complete": false, "exit_code": nil, "wall_seconds": test.wall}
			data, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "interrupted.run.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "interrupted.events.jsonl"), []byte(`{"Action":"output","Package":"fixture","Output":"progress\n"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			report := readReport(directory, []string{"interrupted"})
			data, err = json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Runs []struct {
					WallSeconds *float64 `json:"wall_seconds"`
				}
			}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			wall := decoded.Runs[0].WallSeconds
			if (wall == nil) != (test.wall == nil) || (wall != nil && *wall != *test.wall) {
				t.Fatalf("wall time = %s", data)
			}
			var markdown bytes.Buffer
			writeMarkdown(&markdown, report)
			want := "Command wall time: unavailable"
			if test.wall != nil {
				want = fmt.Sprintf("Command wall time: %.3fs", *test.wall)
			}
			if !strings.Contains(markdown.String(), want) {
				t.Fatalf("wall time not preserved: %s", markdown.String())
			}
		})
	}
}

type blockedProgressWriter struct {
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *blockedProgressWriter) Write(data []byte) (int, error) {
	if len(data) > 0 {
		writer.once.Do(func() { close(writer.ready) })
		<-writer.release
	}
	return len(data), nil
}

func TestRunningCaptureReceiptHasNoInventedWallTimeOrExitCode(t *testing.T) {
	directory := t.TempDir()
	progress := &blockedProgressWriter{ready: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(progress.release) }) }
	defer unblock()
	done := make(chan int, 1)
	go func() {
		done <- capture(directory, "running", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=fail"}, progress, io.Discard)
	}()
	select {
	case <-progress.ready:
	case code := <-done:
		t.Fatalf("capture exited before progress: %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not produce progress")
	}
	data, err := os.ReadFile(filepath.Join(directory, "running.run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Complete    bool
		WallSeconds *float64 `json:"wall_seconds"`
		ExitCode    *int     `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Complete || metadata.WallSeconds != nil || metadata.ExitCode != nil {
		t.Errorf("unfinished capture invented measurements: %s", data)
	}
	unblock()
	select {
	case code := <-done:
		if code != 7 {
			t.Fatalf("child failure was not preserved: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not finish after progress resumed")
	}
}

func TestObservedPackagesAndTestsRequireCompletionEvents(t *testing.T) {
	for _, events := range []string{
		`{"Action":"output","Package":"fixture","Output":"progress\n"}`,
		`{"Action":"output","Package":"fixture","Test":"TestMissing","Output":"progress\n"}
{"Action":"pass","Package":"fixture","Elapsed":1}`,
		`{"Action":"pass","Package":"fixture","Test":"TestOne","Elapsed":1}`,
		`{"Action":"pass","Package":"finished","Elapsed":1}
{"Action":"output","Package":"unfinished","Output":"progress\n"}`,
		`{"Action":"build-output","ImportPath":"fixture","Output":"compiling\n"}`,
	} {
		directory := t.TempDir()
		writeCapture(t, directory, "incomplete", events, true, 0)
		run := readReport(directory, []string{"incomplete"}).Runs[0]
		if run.Complete || len(run.Issues) == 0 {
			t.Fatalf("unterminated event stream marked complete: %s; %+v", events, run)
		}
	}
}

func TestUnavailableDurationsAreNotReportedAsZero(t *testing.T) {
	for _, action := range []string{"pass", "fail", "skip"} {
		for _, elapsed := range []string{"", `,"Elapsed":null`, `,"Elapsed":-1`} {
			for _, target := range []string{"test", "package"} {
				t.Run(action+"/"+target+"/"+elapsed, func(t *testing.T) {
					testElapsed, packageElapsed := `,"Elapsed":0`, `,"Elapsed":0`
					if target == "test" {
						testElapsed = elapsed
					} else {
						packageElapsed = elapsed
					}
					events := fmt.Sprintf(`{"Action":"start","Package":"fixture"}
{"Action":"run","Package":"fixture","Test":"TestOne"}
{"Action":%q,"Package":"fixture","Test":"TestOne"%s}
{"Action":%q,"Package":"fixture"%s}
`, action, testElapsed, action, packageElapsed)
					directory := t.TempDir()
					writeCapture(t, directory, "duration", events, true, 0)
					report := readReport(directory, []string{"duration"})
					run := report.Runs[0]
					if run.Complete || len(run.Issues) == 0 || len(run.Tests) != 1 || len(run.Packages) != 1 {
						t.Fatalf("unavailable duration = %+v", run)
					}
					data, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					var results struct {
						Runs []struct {
							Tests    []struct{ Seconds *float64 }
							Packages []struct{ Seconds *float64 }
						}
					}
					if err := json.Unmarshal(data, &results); err != nil {
						t.Fatal(err)
					}
					seconds := results.Runs[0].Tests[0].Seconds
					if target == "package" {
						seconds = results.Runs[0].Packages[0].Seconds
					}
					if seconds != nil {
						t.Fatalf("unknown duration became %v seconds", *seconds)
					}
					var markdown bytes.Buffer
					writeMarkdown(&markdown, report)
					if !strings.Contains(markdown.String(), "| "+action+" | unavailable |") {
						t.Fatalf("unknown cost not labeled unavailable: %s", markdown.String())
					}
				})
			}
		}
	}
}

func TestCaptureDistinguishesExplicitZeroFromUnavailableDuration(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
	}{
		{"missing", 1},
		{"null", 1},
		{"negative", 1},
		{"zero", 0},
		{"failed-missing", 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			var output, diagnostics bytes.Buffer
			code := capture(directory, "duration", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=duration", "OMNIGREX_TIMING_DURATION=" + test.name}, &output, &diagnostics)
			if code != test.code {
				t.Fatalf("exit code = %d, want %d; diagnostics: %s", code, test.code, diagnostics.String())
			}
			run := readReport(directory, []string{"duration"}).Runs[0]
			if run.Complete != (test.name == "zero") || len(run.Tests) != 1 {
				t.Fatalf("duration capture = %+v", run)
			}
			data, err := json.Marshal(run.Tests[0])
			if err != nil {
				t.Fatal(err)
			}
			want := `"seconds":null`
			if test.name == "zero" {
				want = `"seconds":0`
			}
			if !bytes.Contains(data, []byte(want)) {
				t.Fatalf("reported duration = %s, want %s", data, want)
			}
		})
	}
}

func TestReportRejectsMalformedFixtureRecordsWithoutInventingZeroSamples(t *testing.T) {
	for _, record := range []string{
		`{"fixture":"1","phase":"postgres_ready","elapsed_ns":-1,"outcome":"ok"}`,
		`{"fixture":"1","phase":"postgres_ready","outcome":"ok"}`,
		`{"fixture":"1","phase":"postgres_ready","elapsed_ns":null,"outcome":"ok"}`,
		`{"phase":"postgres_ready","elapsed_ns":1,"outcome":"ok"}`,
	} {
		directory := t.TempDir()
		data, _ := json.Marshal(testEvent{Action: "output", Package: "store", Test: "TestOne", Output: fixtureMarker + record + "\n"})
		events := string(data) + "\n" + `{"Action":"pass","Package":"store","Test":"TestOne","Elapsed":1}
{"Action":"pass","Package":"store","Elapsed":1}`
		writeCapture(t, directory, "integration", events, true, 0)
		run := readReport(directory, []string{"integration"}).Runs[0]
		if run.Complete || len(run.Issues) == 0 || len(run.Phases) != 0 {
			t.Fatalf("malformed timing %s = %+v", record, run)
		}
	}
}

func TestReportUsesNearestRankP95AndRetainsAllSamples(t *testing.T) {
	directory := t.TempDir()
	var events strings.Builder
	for i := 1; i <= 20; i++ {
		output := fmt.Sprintf("fixture_timing {\"fixture\":\"1\",\"phase\":\"store_open\",\"elapsed_ns\":%d,\"outcome\":\"ok\"}\n", int64(i)*1_000_000_000)
		data, _ := json.Marshal(testEvent{Action: "output", Package: "store", Test: "TestRepeated", Output: output})
		fmt.Fprintln(&events, string(data))
	}
	fmt.Fprintln(&events, `{"Action":"pass","Package":"store","Test":"TestRepeated","Elapsed":210}`)
	fmt.Fprintln(&events, `{"Action":"pass","Package":"store","Elapsed":210}`)
	writeCapture(t, directory, "integration", events.String(), true, 0)
	run := readReport(directory, []string{"integration"}).Runs[0]
	if len(run.Samples) != 20 || len(run.Phases) != 1 || run.Phases[0].MedianSeconds != 10.5 || run.Phases[0].P95Seconds != 19 || run.Phases[0].TotalSeconds != 210 {
		t.Fatalf("statistics = %+v", run)
	}
}

func TestCaptureStreamsLargeSuccessfulEvents(t *testing.T) {
	directory := t.TempDir()
	var output, diagnostics bytes.Buffer
	code := capture(directory, "success", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=large"}, &output, &diagnostics)
	if code != 0 || output.String() != strings.Repeat("x", 128<<10)+"\n" {
		t.Fatalf("code=%d output bytes=%d diagnostics=%s", code, output.Len(), diagnostics.String())
	}
	run := readReport(directory, []string{"success"}).Runs[0]
	if !run.Complete || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("success = %+v", run)
	}
}

func TestCaptureStreamsOutputPreservesFailureAndCreatesArtifacts(t *testing.T) {
	directory := t.TempDir()
	var output, diagnostics bytes.Buffer
	code := capture(directory, "failure", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=fail"}, &output, &diagnostics)
	if code != 7 || output.String() != "controlled progress\n" || !strings.Contains(diagnostics.String(), "controlled stderr") {
		t.Fatalf("code=%d output=%q diagnostics=%q", code, output.String(), diagnostics.String())
	}
	for _, name := range []string{"failure.events.jsonl", "failure.run.json"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	run := readReport(directory, []string{"failure"}).Runs[0]
	if !run.Complete || run.ExitCode == nil || *run.ExitCode != 7 || run.WallSeconds == nil || *run.WallSeconds <= 0 {
		t.Fatalf("failure metadata = %+v", run)
	}
}

func TestCaptureReportsMalformedOutputAndLaunchFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		env  []string
	}{
		{"malformed", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=malformed"}},
		{"empty", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=empty"}},
		{"unfinished", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=unfinished"}},
		{"output-only", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=output-only"}},
		{"build-only", []string{os.Args[0], "-test.run=^TestTimingHelperProcess$"}, []string{"OMNIGREX_TIMING_HELPER=build-only"}},
		{"launch", []string{filepath.Join(t.TempDir(), "missing-command")}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			var output, diagnostics bytes.Buffer
			if code := capture(directory, test.name, test.args, test.env, &output, &diagnostics); code != 1 {
				t.Fatalf("code = %d", code)
			}
			run := readReport(directory, []string{test.name}).Runs[0]
			if run.Complete || len(run.Issues) == 0 {
				t.Fatalf("invalid capture = %+v", run)
			}
		})
	}
}

func TestTimingHelperProcess(t *testing.T) {
	switch os.Getenv("OMNIGREX_TIMING_HELPER") {
	case "output-only":
		fmt.Println(`{"Action":"output","Package":"fixture","Output":"controlled progress\n"}`)
		os.Exit(0)
	case "build-only":
		fmt.Println(`{"Action":"build-output","ImportPath":"fixture","Output":"compiling\n"}`)
		os.Exit(0)
	case "duration":
		elapsed := ""
		switch os.Getenv("OMNIGREX_TIMING_DURATION") {
		case "null":
			elapsed = `,"Elapsed":null`
		case "negative":
			elapsed = `,"Elapsed":-1`
		case "zero":
			elapsed = `,"Elapsed":0`
		}
		fmt.Println(`{"Action":"start","Package":"fixture"}`)
		fmt.Println(`{"Action":"run","Package":"fixture","Test":"TestOne"}`)
		fmt.Printf("{\"Action\":\"pass\",\"Package\":\"fixture\",\"Test\":\"TestOne\"%s}\n", elapsed)
		fmt.Println(`{"Action":"pass","Package":"fixture","Elapsed":0}`)
		if os.Getenv("OMNIGREX_TIMING_DURATION") == "failed-missing" {
			os.Exit(7)
		}
		os.Exit(0)
	case "empty":
		os.Exit(0)
	case "unfinished":
		fmt.Println(`{"Action":"run","Package":"fixture","Test":"TestInterrupted"}`)
		os.Exit(0)
	case "large":
		data, _ := json.Marshal(testEvent{Action: "output", Package: "fixture", Output: strings.Repeat("x", 128<<10) + "\n"})
		fmt.Println(string(data))
		fmt.Println(`{"Action":"pass","Package":"fixture","Elapsed":0.01}`)
		os.Exit(0)
	case "fail":
		fmt.Println(`{"Action":"output","Package":"fixture","Output":"controlled progress\n"}`)
		fmt.Println(`{"Action":"fail","Package":"fixture","Elapsed":0.01}`)
		fmt.Fprintln(os.Stderr, "controlled stderr")
		os.Exit(7)
	case "malformed":
		fmt.Print(`{"Action":`)
		os.Exit(0)
	}
}
