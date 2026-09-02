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

func TestMarkerRejectsUnsafeTokensAndIgnoresMalformedText(t *testing.T) {
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
	want := githubapi.Marker{WorkflowID: "valid", OperationID: "op-1"}
	if len(parsed) != 1 || parsed[0] != want {
		t.Errorf("ParseMarkers() = %#v, want only %#v", parsed, want)
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
