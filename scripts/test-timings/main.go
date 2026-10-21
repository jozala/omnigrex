// This source-checkout tool captures go test -json without hiding live output or
// failures. Reports describe observed timings, not a partition of command time.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const fixtureMarker = "fixture_timing "

var validLabel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

type testEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed *float64
	Output  string
}

type captureMetadata struct {
	Label       string    `json:"label"`
	StartedAt   time.Time `json:"started_at"`
	WallSeconds *float64  `json:"wall_seconds"`
	Complete    bool      `json:"complete"`
	ExitCode    *int      `json:"exit_code"`
	Issues      []string  `json:"issues,omitempty"`
}

type result struct {
	Package string   `json:"package"`
	Test    string   `json:"test,omitempty"`
	Outcome string   `json:"outcome"`
	Seconds *float64 `json:"seconds"`
}

type fixtureRecord struct {
	Fixture   string `json:"fixture"`
	Phase     string `json:"phase"`
	ElapsedNS *int64 `json:"elapsed_ns"`
	Outcome   string `json:"outcome"`
}

type phaseSample struct {
	fixtureRecord
	Package string `json:"package"`
	Test    string `json:"test"`
}

type phaseSummary struct {
	Phase         string  `json:"phase"`
	Count         int     `json:"count"`
	Errors        int     `json:"errors"`
	TotalSeconds  float64 `json:"total_seconds"`
	MedianSeconds float64 `json:"median_seconds"`
	P95Seconds    float64 `json:"p95_seconds"`
}

type runReport struct {
	captureMetadata
	Recorded  bool           `json:"recorded"`
	Fixtures  int            `json:"fixture_attempts"`
	Databases int            `json:"database_fixtures"`
	Failed    int            `json:"failed_tests"`
	Skipped   int            `json:"skipped_tests"`
	Packages  []result       `json:"packages"`
	Tests     []result       `json:"tests"`
	Samples   []phaseSample  `json:"fixture_samples"`
	Phases    []phaseSummary `json:"phases"`
}

type timingReport struct {
	Runs []runReport `json:"runs"`
}

func main() {
	if len(os.Args) >= 6 && os.Args[1] == "run" && os.Args[4] == "--" {
		os.Exit(capture(os.Args[2], os.Args[3], os.Args[5:], nil, os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 3 && os.Args[1] == "report" {
		directory := os.Args[2]
		labels := os.Args[3:]
		if len(labels) == 0 {
			fmt.Fprintln(os.Stderr, "report requires the expected capture labels")
			os.Exit(2)
		}
		for _, label := range labels {
			if !validLabel.MatchString(label) {
				fmt.Fprintln(os.Stderr, "invalid capture label")
				os.Exit(2)
			}
		}
		if err := os.MkdirAll(directory, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		report := readReport(directory, labels)
		if err := writeJSON(filepath.Join(directory, "summary.json"), report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		file, err := os.Create(filepath.Join(directory, "summary.md"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var markdown bytes.Buffer
		writeMarkdown(&markdown, report)
		_, writeErr := io.MultiWriter(file, os.Stdout).Write(markdown.Bytes())
		if err := errors.Join(writeErr, file.Close()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "invalid test-timings arguments")
	}
	fmt.Fprintln(os.Stderr, "usage: test-timings run DIRECTORY LABEL -- COMMAND [ARGS...] | report DIRECTORY LABEL...")
	os.Exit(2)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Publish metadata atomically so an interrupted command leaves either its
	// initial incomplete marker or the final receipt, not partially written JSON.
	staged, err := os.CreateTemp(filepath.Dir(path), ".timings-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	_, writeErr := staged.Write(append(data, '\n'))
	if err := errors.Join(writeErr, staged.Close()); err != nil {
		return err
	}
	return os.Rename(staged.Name(), path)
}

func capture(directory, label string, args, extraEnvironment []string, stdout, stderr io.Writer) (code int) {
	if !validLabel.MatchString(label) || len(args) == 0 {
		fmt.Fprintln(stderr, "capture requires a safe label and command")
		return 2
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	metadata := captureMetadata{Label: label, StartedAt: time.Now().UTC()}
	metadataPath := filepath.Join(directory, label+".run.json")
	if err := writeJSON(metadataPath, metadata); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	start := time.Now()
	code = 1
	finished := false
	defer func() {
		// A killed wrapper never executes this, leaving Complete=false on disk.
		if finished {
			wall := time.Since(start).Seconds()
			metadata.WallSeconds = &wall
			metadata.ExitCode = &code
		}
		if err := writeJSON(metadataPath, metadata); err != nil {
			fmt.Fprintln(stderr, "write capture metadata:", err)
			if code == 0 {
				code = 1
			}
		}
	}()
	raw, err := os.Create(filepath.Join(directory, label+".events.jsonl"))
	if err != nil {
		metadata.Issues = append(metadata.Issues, "could not create event capture")
		fmt.Fprintln(stderr, err)
		return code
	}
	command := exec.Command(args[0], args[1:]...)
	command.Env = append(os.Environ(), extraEnvironment...)
	command.Stderr = stderr
	pipe, err := command.StdoutPipe()
	if err != nil {
		raw.Close()
		metadata.Issues = append(metadata.Issues, "could not open command output")
		fmt.Fprintln(stderr, err)
		return code
	}
	if err := command.Start(); err != nil {
		pipe.Close()
		raw.Close()
		metadata.Issues = append(metadata.Issues, "command did not start")
		fmt.Fprintln(stderr, err)
		return code
	}
	events := 0
	var executions executionTracker
	readEvents(io.TeeReader(pipe, raw), func(event testEvent) {
		events++
		executions.observe(event)
		if _, err := io.WriteString(stdout, event.Output); err != nil {
			metadata.Issues = append(metadata.Issues, "could not stream test progress")
		}
	}, func(issue string) { metadata.Issues = append(metadata.Issues, issue) })
	// Even if writing the raw artifact fails, drain the pipe before Wait so
	// the test process cannot block forever on a full stdout pipe.
	io.Copy(io.Discard, pipe)
	err = command.Wait()
	finished = true
	if closeErr := raw.Close(); closeErr != nil {
		metadata.Issues = append(metadata.Issues, "could not finish event capture")
	}
	if events == 0 {
		metadata.Issues = append(metadata.Issues, "no Go test events captured")
	}
	metadata.Issues = append(metadata.Issues, executions.issues()...)
	metadata.Complete = true
	code = 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			code = exit.ExitCode()
		} else {
			code = 1
			metadata.Issues = append(metadata.Issues, "command did not exit normally")
		}
	}
	if len(metadata.Issues) > 0 {
		metadata.Complete = false
		fmt.Fprintln(stderr, "test timing capture is incomplete:", strings.Join(metadata.Issues, "; "))
		if code == 0 {
			code = 1
		}
	}
	return code
}

func readEvents(reader io.Reader, consume func(testEvent), issue func(string)) {
	input := bufio.NewReader(reader)
	for {
		line, err := input.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var event testEvent
			if decodeErr := json.Unmarshal(line, &event); decodeErr != nil || event.Action == "" {
				issue("malformed or truncated Go test event")
			} else {
				if event.Package == "" && !strings.HasPrefix(event.Action, "build-") {
					issue("Go test event has no package")
				}
				switch event.Action {
				case "pass", "fail", "skip":
					if event.Elapsed == nil || *event.Elapsed < 0 {
						issue(fmt.Sprintf("unavailable elapsed duration for %s/%s (%s)", event.Package, event.Test, event.Action))
						event.Elapsed = nil
					}
				}
				consume(event)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				issue("could not read complete Go test output")
			}
			return
		}
	}
}

func readReport(directory string, labels []string) timingReport {
	report := timingReport{}
	for _, label := range labels {
		run := runReport{captureMetadata: captureMetadata{Label: label}}
		data, err := os.ReadFile(filepath.Join(directory, label+".run.json"))
		if err != nil || json.Unmarshal(data, &run.captureMetadata) != nil {
			run.Issues = append(run.Issues, "capture metadata unavailable or invalid")
		} else {
			run.Recorded = true
			if run.Complete && (run.WallSeconds == nil || *run.WallSeconds < 0 || run.ExitCode == nil) {
				run.Issues = append(run.Issues, "completed capture has unavailable wall time or exit code")
			}
		}
		if !run.Complete {
			run.Issues = append(run.Issues, "command completion was not recorded")
		}
		file, err := os.Open(filepath.Join(directory, label+".events.jsonl"))
		if err != nil {
			run.Issues = append(run.Issues, "test events unavailable; command may not have run")
		} else {
			analyzeEvents(file, &run)
			file.Close()
		}
		run.Complete = run.Complete && len(run.Issues) == 0
		report.Runs = append(report.Runs, run)
	}
	return report
}

func analyzeEvents(reader io.Reader, run *runReport) {
	var executions executionTracker
	fixtures := map[string]bool{}
	databases := map[string]bool{}
	phaseValues := map[string][]float64{}
	phaseErrors := map[string]int{}
	events := 0
	readEvents(reader, func(event testEvent) {
		events++
		key := event.Package + "\x00" + event.Test
		executions.observe(event)
		switch event.Action {
		case "pass", "fail", "skip":
			item := result{Package: event.Package, Test: event.Test, Outcome: event.Action, Seconds: event.Elapsed}
			if event.Test == "" {
				run.Packages = append(run.Packages, item)
			} else {
				run.Tests = append(run.Tests, item)
				if event.Action == "fail" {
					run.Failed++
				}
				if event.Action == "skip" {
					run.Skipped++
				}
			}
		}
		index := strings.Index(event.Output, fixtureMarker)
		if index < 0 {
			return
		}
		var record fixtureRecord
		if err := json.Unmarshal([]byte(strings.TrimSpace(event.Output[index+len(fixtureMarker):])), &record); err != nil || record.Phase == "" || record.ElapsedNS == nil || *record.ElapsedNS < 0 || (record.Outcome != "ok" && record.Outcome != "error") || (strings.HasPrefix(record.Phase, "postgres_") && record.Fixture == "") {
			run.Issues = append(run.Issues, "invalid fixture timing record")
			return
		}
		run.Samples = append(run.Samples, phaseSample{fixtureRecord: record, Package: event.Package, Test: event.Test})
		phaseValues[record.Phase] = append(phaseValues[record.Phase], float64(*record.ElapsedNS)/float64(time.Second))
		if record.Outcome == "error" {
			phaseErrors[record.Phase]++
		}
		if record.Phase == "postgres_start" {
			fixtures[key+"\x00"+record.Fixture] = true
		}
		if record.Phase == "postgres_database_create" {
			databases[key+"\x00"+record.Fixture] = true
		}
	}, func(issue string) { run.Issues = append(run.Issues, issue) })
	if events == 0 {
		run.Issues = append(run.Issues, "no Go test events captured")
	}
	run.Issues = append(run.Issues, executions.issues()...)
	run.Fixtures = len(fixtures)
	run.Databases = len(databases)
	for phase, values := range phaseValues {
		slices.Sort(values)
		middle := len(values) / 2
		median := values[middle]
		if len(values)%2 == 0 {
			median = (values[middle-1] + values[middle]) / 2
		}
		var total float64
		for _, value := range values {
			total += value
		}
		run.Phases = append(run.Phases, phaseSummary{Phase: phase, Count: len(values), Errors: phaseErrors[phase], TotalSeconds: total, MedianSeconds: median, P95Seconds: values[int(math.Ceil(float64(len(values))*0.95))-1]})
	}
	slices.SortFunc(run.Phases, func(a, b phaseSummary) int { return strings.Compare(a.Phase, b.Phase) })
}

type executionTracker struct {
	tests    map[string]bool
	packages map[string]bool
}

func (tracker *executionTracker) observe(event testEvent) {
	if event.Package == "" {
		return
	}
	if tracker.packages == nil {
		tracker.packages = map[string]bool{}
		tracker.tests = map[string]bool{}
	}
	// Even when a start/run event is missing, output proves that this package
	// (and any named test) was observed and still needs a terminal event.
	tracker.packages[event.Package] = false
	key := event.Package + "\x00" + event.Test
	switch event.Action {
	case "pass", "fail", "skip":
		if event.Test == "" {
			tracker.packages[event.Package] = true
		} else {
			delete(tracker.tests, key)
		}
	default:
		if event.Test != "" {
			tracker.tests[key] = true
		}
	}
}

func (tracker *executionTracker) issues() []string {
	var issues []string
	if len(tracker.packages) == 0 {
		issues = append(issues, "no Go test packages observed")
	}
	unfinished := 0
	for _, complete := range tracker.packages {
		if !complete {
			unfinished++
		}
	}
	if unfinished > 0 {
		issues = append(issues, fmt.Sprintf("%d observed packages have no terminal event", unfinished))
	}
	if len(tracker.tests) > 0 {
		issues = append(issues, fmt.Sprintf("%d observed tests have no terminal event", len(tracker.tests)))
	}
	return issues
}

func writeMarkdown(writer io.Writer, report timingReport) {
	fmt.Fprint(writer, "# PR check test timings\n\n")
	fmt.Fprint(writer, "Command wall time includes compilation and test execution, not preceding image builds. Package/test timings overlap; summed samples are work, not wall time. Parent test durations include subtests.\n\n")
	fmt.Fprint(writer, "PostgreSQL phases measure fixture operations. store_open includes connections and migration handling; migration probes measure fresh/existing schemas separately and are not an attribution of every store_open call.\n\n")
	fmt.Fprint(writer, "Unmeasured here: Docker-contained Workflow, recovery, upgrade, deployment and development-image qualification; image builds; individual SQL calls; fixture phases that did not finish. No residual is labeled as test-body time.\n\n")
	for _, run := range report.Runs {
		fmt.Fprintf(writer, "## %s\n\n", markdownCell(run.Label))
		status := "complete"
		if !run.Complete {
			status = "INCOMPLETE"
		}
		wall, exit := "unavailable", "unavailable"
		if run.Recorded && run.WallSeconds != nil && *run.WallSeconds >= 0 {
			wall = fmt.Sprintf("%.3fs", *run.WallSeconds)
		}
		if run.ExitCode != nil {
			exit = fmt.Sprint(*run.ExitCode)
		}
		fmt.Fprintf(writer, "Capture: **%s**. Command wall time: %s. Exit code: %s. PostgreSQL container start attempts: %d. Database creation attempts: %d. Failed test records: %d. Skipped test records: %d.\n\n", status, wall, exit, run.Fixtures, run.Databases, run.Failed, run.Skipped)
		for _, issue := range run.Issues {
			fmt.Fprintf(writer, "- %s\n", markdownCell(issue))
		}
		fmt.Fprintln(writer)
		writeResults(writer, "Package execution", run.Packages, func(result) bool { return true })
		writeResults(writer, "Slowest top-level tests", run.Tests, func(item result) bool { return !strings.Contains(item.Test, "/") })
		writeResults(writer, "Slowest subtests (included in their parents)", run.Tests, func(item result) bool { return strings.Contains(item.Test, "/") })
		if len(run.Phases) == 0 {
			fmt.Fprint(writer, "No completed fixture phase samples were captured; this does not establish zero fixture cost.\n\n")
			continue
		}
		fmt.Fprintln(writer, "### Fixture phases\n\n| Phase | Samples | Errors | Total work (s) | Median (s) | p95 (s) |\n| --- | ---: | ---: | ---: | ---: | ---: |")
		for _, phase := range run.Phases {
			fmt.Fprintf(writer, "| %s | %d | %d | %.3f | %.3f | %.3f |\n", markdownCell(phase.Phase), phase.Count, phase.Errors, phase.TotalSeconds, phase.MedianSeconds, phase.P95Seconds)
		}
		fmt.Fprintln(writer)
		// Aggregate only fixture phases, not the separate migration probes. Do
		// not roll child samples into parents, which would count them twice.
		byTest := map[string]result{}
		for _, sample := range run.Samples {
			if strings.HasPrefix(sample.Phase, "probe_") {
				continue
			}
			key := sample.Package + "\x00" + sample.Test
			item := byTest[key]
			item.Package, item.Test, item.Outcome = sample.Package, sample.Test, "measured work"
			if item.Seconds == nil {
				item.Seconds = new(float64)
			}
			*item.Seconds += float64(*sample.ElapsedNS) / float64(time.Second)
			byTest[key] = item
		}
		var fixtureCosts []result
		for _, item := range byTest {
			fixtureCosts = append(fixtureCosts, item)
		}
		writeResults(writer, "Highest measured fixture costs by owning test", fixtureCosts, func(result) bool { return true })
	}
}

func writeResults(writer io.Writer, title string, items []result, include func(result) bool) {
	filtered := make([]result, 0, len(items))
	for _, item := range items {
		if include(item) {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return
	}
	slices.SortFunc(filtered, func(a, b result) int {
		if a.Seconds == nil && b.Seconds != nil {
			return 1
		}
		if a.Seconds != nil && b.Seconds == nil {
			return -1
		}
		if a.Seconds != nil && b.Seconds != nil && *a.Seconds != *b.Seconds {
			if *a.Seconds > *b.Seconds {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Package+"/"+a.Test, b.Package+"/"+b.Test)
	})
	fmt.Fprintf(writer, "### %s\n\n| Package | Test | Outcome | Seconds |\n| --- | --- | --- | ---: |\n", title)
	for _, item := range filtered[:min(20, len(filtered))] {
		seconds := "unavailable"
		if item.Seconds != nil {
			seconds = fmt.Sprintf("%.3f", *item.Seconds)
		}
		fmt.Fprintf(writer, "| %s | %s | %s | %s |\n", markdownCell(item.Package), markdownCell(item.Test), markdownCell(item.Outcome), seconds)
	}
	fmt.Fprintln(writer)
}

func markdownCell(value string) string {
	return strings.NewReplacer("|", "&#124;", "\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;", "`", "&#96;").Replace(value)
}
