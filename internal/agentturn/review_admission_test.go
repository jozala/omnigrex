package agentturn

import (
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/reviewpolicy"
	"github.com/jozala/omnigrex/internal/store"
)

func TestReviewSchedulingQualifiedResults(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	request := store.ReviewAdmission{ID: "request", Generation: 1, HeadSHA: "head", CreatedAt: now, GraceAnchor: now, Deadline: now.Add(30 * time.Minute)}
	mapping := githubapi.CITargetMapping{ReviewedHead: "head", State: "verified"}
	inventory := githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "build", AppID: 7, Source: "classic"}}}
	for _, conclusion := range []string{"success", "failure", "cancelled", "timed_out", "skipped", "neutral", "action_required", "stale", "startup_failure", "unknown"} {
		t.Run(conclusion, func(t *testing.T) {
			observed := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "head", Checks: []githubapi.CheckRun{{ID: 1, Name: "build", App: &githubapi.CheckApp{ID: 7}, Status: "completed", Conclusion: conclusion}}}
			evidence := evaluateReviewCI(reviewpolicy.Default(), inventory, mapping, []githubapi.CIObservation{observed}, request, now, request.Deadline)
			want := "failed_ci"
			if conclusion == "success" || conclusion == "neutral" || conclusion == "skipped" {
				want = "completed_ci"
			}
			if conclusion == "unknown" {
				want = "waiting"
			}
			if evidence.Reason != want {
				t.Fatalf("terminal %s did not release: %+v", conclusion, evidence)
			}
			if conclusion != "success" && evidence.Targets[0].Results[0].State != githubapi.CITerminalNonSuccess {
				t.Fatal("completion became passing qualification")
			}
			observed.Checks = append(observed.Checks, githubapi.CheckRun{ID: 2, Name: "build", App: &githubapi.CheckApp{ID: 7}, Status: "queued"})
			if e := evaluateReviewCI(reviewpolicy.Default(), inventory, mapping, []githubapi.CIObservation{observed}, request, now, request.Deadline); e.Reason != "waiting" {
				t.Fatal("old terminal run released newer queued rerun")
			}
		})
	}
}
func TestReviewSchedulingGraceFailuresAndAbsoluteDeadline(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	r := store.ReviewAdmission{ID: "request", Generation: 1, GraceAnchor: now, Deadline: now.Add(30 * time.Minute)}
	mapping := githubapi.CITargetMapping{ReviewedHead: "head", State: "head_only"}
	empty := githubapi.CIInventory{Availability: githubapi.CIAvailable}
	observation := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "head"}
	for _, test := range []struct {
		name     string
		elapsed  time.Duration
		expected string
		mutate   func(*githubapi.CIInventory, *githubapi.CIObservation, *githubapi.CITargetMapping)
	}{
		{name: "no CI before grace", elapsed: 59 * time.Second, expected: "waiting"},
		{name: "no CI after grace", elapsed: time.Minute, expected: "no_ci_after_grace"},
		{name: "fast check before grace", elapsed: 59 * time.Second, expected: "waiting", mutate: func(_ *githubapi.CIInventory, o *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			o.Checks = []githubapi.CheckRun{{ID: 1, Name: "fast", Status: "completed", Conclusion: "success", App: &githubapi.CheckApp{ID: 7}}}
		}},
		{name: "required missing after grace", elapsed: time.Minute, expected: "waiting", mutate: func(i *githubapi.CIInventory, _ *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			i.Requirements = []githubapi.CIRequirement{{Context: "missing"}}
		}},
		{name: "missing at deadline", elapsed: 30 * time.Minute, expected: "timeout_incomplete", mutate: func(i *githubapi.CIInventory, _ *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			i.Requirements = []githubapi.CIRequirement{{Context: "missing"}}
		}},
		{name: "inaccessible inventory is not empty", elapsed: time.Minute, expected: "waiting", mutate: func(i *githubapi.CIInventory, _ *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			i.Availability = githubapi.CIUnavailable
		}},
		{name: "unresolved rule", elapsed: time.Minute, expected: "waiting", mutate: func(i *githubapi.CIInventory, _ *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			i.Unresolved = []githubapi.CIUnresolvedRule{{Type: "workflows"}}
		}},
		{name: "partial pagination", elapsed: time.Minute, expected: "waiting", mutate: func(_ *githubapi.CIInventory, o *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			o.Availability = githubapi.CIPartial
		}},
		{name: "unknown mergeability is not no CI", elapsed: time.Minute, expected: "waiting", mutate: func(_ *githubapi.CIInventory, _ *githubapi.CIObservation, m *githubapi.CITargetMapping) {
			m.Failures = []githubapi.CIEvidenceFailure{{Operation: "test_merge_mapping", Code: "mergeability_pending"}}
		}},
		{name: "missing merge mapping", elapsed: time.Minute, expected: "waiting", mutate: func(_ *githubapi.CIInventory, _ *githubapi.CIObservation, m *githubapi.CITargetMapping) {
			m.State = "unavailable"
		}},
		{name: "failed discovery deadline", elapsed: 30 * time.Minute, expected: "timeout_incomplete", mutate: func(i *githubapi.CIInventory, _ *githubapi.CIObservation, _ *githubapi.CITargetMapping) {
			i.Availability = githubapi.CIUnavailable
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			i, o, m := empty, observation, mapping
			if test.mutate != nil {
				test.mutate(&i, &o, &m)
			}
			got := evaluateReviewCI(reviewpolicy.Default(), i, m, []githubapi.CIObservation{o}, r, now.Add(test.elapsed), r.Deadline)
			if got.Reason != test.expected {
				t.Fatalf("reason = %s, want %s", got.Reason, test.expected)
			}
		})
	}
	p := reviewpolicy.Default()
	p.Start = "immediate"
	if e := evaluateReviewCI(p, githubapi.CIInventory{}, githubapi.CITargetMapping{}, nil, r, now, r.Deadline); e.Reason != "immediate" {
		t.Fatal("immediate waited")
	}
}
func TestReviewSchedulingExplicitSourcesAndTestMerge(t *testing.T) {
	now := time.Now()
	r := store.ReviewAdmission{GraceAnchor: now, Deadline: now.Add(time.Hour)}
	p := reviewpolicy.Default()
	p.Selection = "explicit"
	p.Checks = []reviewpolicy.Selector{{Context: "build", Source: "check", AppID: 7}}
	i := githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "build", AppID: 7, Source: "repository_explicit", SourceType: "check"}}}
	m := githubapi.CITargetMapping{State: "verified", ReviewedHead: "head", TestMergeSHA: "merge", BaseSHA: "base"}
	head := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "head", Statuses: []githubapi.CommitStatus{{ID: 2, Context: "build", State: "pending"}}}
	merge := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "merge", Checks: []githubapi.CheckRun{{ID: 3, Name: "build", App: &githubapi.CheckApp{ID: 7}, Status: "completed", Conclusion: "failure"}}}
	e := evaluateReviewCI(p, i, m, []githubapi.CIObservation{head, merge}, r, now, r.Deadline)
	if e.Reason != "failed_ci" || e.Targets[1].Commit != "merge" {
		t.Fatalf("test merge evidence = %+v", e)
	}
	head.Checks = []githubapi.CheckRun{{ID: 4, Name: "build", App: &githubapi.CheckApp{ID: 7}, Status: "in_progress"}}
	if e = evaluateReviewCI(p, i, m, []githubapi.CIObservation{head, merge}, r, now, r.Deadline); e.Reason != "waiting" {
		t.Fatal("pending authorized target ignored")
	}
}

func TestReviewSchedulingFailurePrecedesUnrelatedIncompleteEvidence(t *testing.T) {
	now := time.Now().UTC()
	r := store.ReviewAdmission{Generation: 1, GraceAnchor: now, Deadline: now.Add(2 * time.Hour)}
	i := githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "build"}, {Context: "tests"}}}
	m := githubapi.CITargetMapping{State: "head_only", ReviewedHead: "head"}
	for _, state := range []string{"failure", "error", "unknown", "pending", "success"} {
		t.Run(state, func(t *testing.T) {
			o := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "head", Statuses: []githubapi.CommitStatus{{ID: 1, Context: "build", State: state}, {ID: 2, Context: "tests", State: "pending"}}}
			e := evaluateReviewCI(reviewpolicy.Default(), i, m, []githubapi.CIObservation{o}, r, now, r.Deadline)
			want := store.ReviewWait
			if state == "failure" || state == "error" {
				want = store.ReviewRepair
			}
			if e.Disposition != want || e.AllSuccessful {
				t.Fatalf("%s: %+v", state, e)
			}
		})
	}
}

func TestReviewSchedulingSuccessResetExclusionsAndCollisions(t *testing.T) {
	now := time.Now().UTC()
	p := reviewpolicy.Default()
	r := store.ReviewAdmission{Generation: 1, GraceAnchor: now.Add(-time.Minute), Deadline: now.Add(2 * time.Hour)}
	i := githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "build"}}}
	m := githubapi.CITargetMapping{State: "verified", ReviewedHead: "head", TestMergeSHA: "merge"}
	for _, conclusion := range []string{"success", "neutral", "skipped"} {
		t.Run(conclusion, func(t *testing.T) {
			o := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "head", Checks: []githubapi.CheckRun{{ID: 1, Name: "build", App: &githubapi.CheckApp{ID: 7}, Status: "completed", Conclusion: conclusion}}}
			e := evaluateReviewCI(p, i, m, []githubapi.CIObservation{o}, r, now, r.Deadline)
			if e.Disposition != store.ReviewAdmit || e.AllSuccessful != (conclusion == "success") {
				t.Fatalf("eligibility/reset: %+v", e)
			}
			// Success from another producer or authorized target cannot hide a failure.
			other := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "merge", Checks: []githubapi.CheckRun{{ID: 2, Name: "build", App: &githubapi.CheckApp{ID: 8}, Status: "completed", Conclusion: "failure"}}}
			if e := evaluateReviewCI(p, i, m, []githubapi.CIObservation{o, other}, r, now, r.Deadline); e.Disposition != store.ReviewRepair {
				t.Fatal("collision failure hidden", e)
			}
			other.Checks = append(other.Checks, githubapi.CheckRun{ID: 3, Name: "build", App: &githubapi.CheckApp{ID: 8}, Status: "queued"})
			if e := evaluateReviewCI(p, i, m, []githubapi.CIObservation{o, other}, r, now, r.Deadline); e.Disposition != store.ReviewWait {
				t.Fatal("superseded failure routed", e)
			}
		})
	}
	for _, immediate := range []bool{false, true} {
		empty := githubapi.CIInventory{Availability: githubapi.CIAvailable}
		if immediate {
			p.Start = "immediate"
		}
		e := evaluateReviewCI(p, empty, m, []githubapi.CIObservation{{Availability: githubapi.CIAvailable}}, r, now, r.Deadline)
		if e.Disposition != store.ReviewAdmit || e.AllSuccessful {
			t.Fatal("no-CI/bypass reset", e)
		}
	}
}

func TestReviewSchedulingEstablishedFailureWithPartialDiscovery(t *testing.T) {
	now := time.Now()
	r := store.ReviewAdmission{Deadline: now.Add(time.Hour)}
	i := githubapi.CIInventory{Availability: githubapi.CIPartial, Requirements: []githubapi.CIRequirement{{Context: "build", Source: "classic"}}, Failures: []githubapi.CIEvidenceFailure{{Operation: "effective_rules", Code: "unavailable"}}}
	m := githubapi.CITargetMapping{State: "head_only", ReviewedHead: "head"}
	o := githubapi.CIObservation{Availability: githubapi.CIAvailable, HeadSHA: "head", Statuses: []githubapi.CommitStatus{{ID: 1, Context: "build", State: "failure"}}}
	if e := evaluateReviewCI(reviewpolicy.Default(), i, m, []githubapi.CIObservation{o}, r, now, r.Deadline); e.Disposition != store.ReviewRepair {
		t.Fatal("known required failure lost to unrelated unavailable discovery", e)
	}
	i.Requirements = nil
	if e := evaluateReviewCI(reviewpolicy.Default(), i, m, []githubapi.CIObservation{o}, r, now, r.Deadline); e.Disposition != store.ReviewWait {
		t.Fatal("unknown discovery invented selected failure", e)
	}
}
