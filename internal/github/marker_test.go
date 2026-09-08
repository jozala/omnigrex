package github_test

import (
	"errors"
	"strings"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestMarkerRendersAndParsesVersionedGrammar(t *testing.T) {
	marker := githubapi.Marker{
		WorkflowID:        "workflow-019a",
		AgentAssignmentID: "assignment_7",
		OperationID:       "operation:42.1",
	}
	rendered, err := githubapi.RenderMarker(marker)
	if err != nil {
		t.Fatalf("RenderMarker() error = %v", err)
	}
	want := "<!-- omnigrex:v1 workflow=workflow-019a assignment=assignment_7 operation=operation:42.1 -->"
	if rendered != want {
		t.Errorf("RenderMarker() = %q, want %q", rendered, want)
	}
	parsed := githubapi.ParseMarkers("visible user text\n\n" + rendered)
	if len(parsed) != 1 || parsed[0] != marker {
		t.Errorf("ParseMarkers() = %#v, want %#v", parsed, marker)
	}
	if githubapi.MarkerVersion != "v1" {
		t.Errorf("MarkerVersion = %q, want v1", githubapi.MarkerVersion)
	}
}

func TestMarkerSupportsOptionalAssignmentAndOperation(t *testing.T) {
	markers := []githubapi.Marker{
		{WorkflowID: "workflow-1"},
		{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1"},
		{WorkflowID: "workflow-1", OperationID: "operation-1"},
	}
	var text strings.Builder
	for _, marker := range markers {
		rendered, err := githubapi.RenderMarker(marker)
		if err != nil {
			t.Fatalf("RenderMarker(%#v) error = %v", marker, err)
		}
		text.WriteString(rendered)
		text.WriteByte('\n')
	}
	if got := githubapi.ParseMarkers(text.String()); len(got) != len(markers) {
		t.Fatalf("ParseMarkers() = %#v", got)
	} else {
		for index := range markers {
			if got[index] != markers[index] {
				t.Errorf("marker %d = %#v, want %#v", index, got[index], markers[index])
			}
		}
	}
}

func TestMarkerRejectsUnsafeTokensAndDoesNotTrustValidNeighborsOfMalformedText(t *testing.T) {
	unsafeMarkers := []githubapi.Marker{
		{},
		{WorkflowID: "contains space"},
		{WorkflowID: "workflow/1"},
		{WorkflowID: "-starts-with-punctuation"},
		{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-->"},
		{WorkflowID: "workflow-1", OperationID: strings.Repeat("a", 129)},
	}
	for _, marker := range unsafeMarkers {
		if _, err := githubapi.RenderMarker(marker); !errors.Is(err, githubapi.ErrInvalidMarkerToken) {
			t.Errorf("RenderMarker(%#v) error = %v, want ErrInvalidMarkerToken", marker, err)
		}
	}

	text := strings.Join([]string{
		"ordinary <!-- user comment --> text",
		"<!-- omnigrex:v2 workflow=future -->",
		"<!-- omnigrex:v1 workflow=bad/value -->",
		"<!-- omnigrex:v1 operation=missing-workflow -->",
		"<!-- omnigrex:v1 workflow=valid operation=op-1 -->",
		"<!-- omnigrex:v1  workflow=extra-space -->",
		"<!-- omnigrex:v1 workflow=valid operation=op-1 unexpected=value -->",
		"unfinished <!-- omnigrex:v1 workflow=ignored",
	}, "\n")
	parsed := githubapi.ParseMarkers(text)
	if len(parsed) != 0 {
		t.Errorf("ParseMarkers() = %#v, want no trusted markers from untrusted text", parsed)
	}
	if _, found, err := githubapi.FindMarker(text, githubapi.Marker{OperationID: "op-1"}); err != nil || found {
		t.Errorf("FindMarker() = (_, %t, %v), want no binding from untrusted text", found, err)
	}
}

func TestInspectMarkersMarksMalformedOmnigrexCommentsUntrusted(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unsupported version", body: `<!-- omnigrex:v2 workflow=workflow-1 -->`},
		{name: "unclosed", body: `<!-- omnigrex:v1 workflow=workflow-1`},
		{name: "invalid token", body: `<!-- omnigrex:v1 workflow=bad/value -->`},
		{name: "unknown field", body: `<!-- omnigrex:v1 workflow=workflow-1 owner=user -->`},
		{name: "missing colon", body: `<!-- omnigrex v1 workflow=workflow-1 -->`},
		{name: "malformed spacing", body: `<!-- omnigrex:v1  workflow=workflow-1 -->`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := githubapi.InspectMarkers(test.body)
			if !inspection.Untrusted || len(inspection.Markers) != 0 {
				t.Errorf("InspectMarkers() = %#v, want untrusted without parsed markers", inspection)
			}
		})
	}

	inspection := githubapi.InspectMarkers(strings.Join([]string{
		`<!-- omnigrex:v1 workflow=workflow-1 -->`,
		`<!-- omnigrex:v1 workflow=bad/value -->`,
	}, "\n"))
	if !inspection.Untrusted || len(inspection.Markers) != 1 || inspection.Markers[0].WorkflowID != "workflow-1" {
		t.Errorf("mixed marker inspection = %#v, want valid observation plus untrusted marker state", inspection)
	}
}

func TestEnsureMarkerRejectsUntrustedOmnigrexLikeText(t *testing.T) {
	marker := githubapi.Marker{WorkflowID: "workflow-1", OperationID: "operation-1"}
	for _, body := range []string{
		`<!-- omnigrex:v2 workflow=workflow-1 -->`,
		`<!-- omnigrex:v1 workflow=workflow-1`,
		`<!-- omnigrex:v1 workflow=bad/value -->`,
		`<!-- omnigrex:v1 workflow=workflow-1 owner=user -->`,
		`<!-- omnigrex v1 workflow=workflow-1 -->`,
	} {
		if got, err := githubapi.EnsureMarker(body, marker); !errors.Is(err, githubapi.ErrUntrustedMarkerText) || got != "" {
			t.Errorf("EnsureMarker(%q) = (%q, %v), want ErrUntrustedMarkerText without output", body, got, err)
		}
	}
}

func TestEnsureMarkerAvoidsIdenticalDuplicatesAndSupportsLookup(t *testing.T) {
	marker := githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: "operation-1"}
	withMarker, err := githubapi.EnsureMarker("review body", marker)
	if err != nil {
		t.Fatalf("EnsureMarker() error = %v", err)
	}
	want := "review body\n\n<!-- omnigrex:v1 workflow=workflow-1 assignment=assignment-1 operation=operation-1 -->"
	if withMarker != want {
		t.Errorf("EnsureMarker() = %q, want %q", withMarker, want)
	}
	again, err := githubapi.EnsureMarker(withMarker, marker)
	if err != nil {
		t.Fatalf("second EnsureMarker() error = %v", err)
	}
	if again != withMarker {
		t.Errorf("second EnsureMarker() duplicated marker: %q", again)
	}

	found, ok, err := githubapi.FindMarker(withMarker, githubapi.Marker{OperationID: "operation-1"})
	if err != nil {
		t.Fatalf("FindMarker() error = %v", err)
	}
	if !ok || found != marker {
		t.Errorf("FindMarker() = %#v, %t; want %#v, true", found, ok, marker)
	}
	if _, ok, err := githubapi.FindMarker(withMarker, githubapi.Marker{WorkflowID: "other"}); err != nil || ok {
		t.Errorf("FindMarker(other) found = %t, error = %v", ok, err)
	}
	if _, _, err := githubapi.FindMarker(withMarker, githubapi.Marker{OperationID: "unsafe value"}); !errors.Is(err, githubapi.ErrInvalidMarkerToken) {
		t.Errorf("FindMarker(unsafe) error = %v, want ErrInvalidMarkerToken", err)
	}
}

func TestMarkerLookupSurvivesUnrelatedUnclosedHTMLComment(t *testing.T) {
	marker := githubapi.Marker{WorkflowID: "workflow-123", OperationID: "operation-789"}
	rendered, err := githubapi.RenderMarker(marker)
	if err != nil {
		t.Fatalf("RenderMarker() error = %v", err)
	}
	text := "<!-- unrelated comment without a terminator\nuser text\n" + rendered
	got, found, err := githubapi.FindMarker(text, marker)
	if err != nil {
		t.Fatalf("FindMarker() error = %v", err)
	}
	if !found || got != marker {
		t.Errorf("FindMarker() = (%#v, %v), want marker after unrelated comment", got, found)
	}
	ensured, err := githubapi.EnsureMarker(text, marker)
	if err != nil {
		t.Fatalf("EnsureMarker() error = %v", err)
	}
	if ensured != text {
		t.Error("EnsureMarker() duplicated a marker hidden by unrelated HTML")
	}
}
