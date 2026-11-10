package piresources

import (
	"encoding/json"
	"testing"
)

func TestCandidateAndQualifiedIdentitiesAreSeparate(t *testing.T) {
	selected, err := SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	proposed := selected.Candidate()
	proposed.Resources = CandidateContract("amd64")
	data, _ := json.Marshal(proposed)
	if _, err := ParseCandidate(data); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBundle(data); err == nil {
		t.Fatal("unqualified candidate admitted as production")
	}
	for _, mutate := range []func(*CandidateBundle){func(c *CandidateBundle) { c.Image = "ghcr.io/jozala/omnigrex/pi:latest" }, func(c *CandidateBundle) { c.SourceCommit = "unknown" }, func(c *CandidateBundle) { c.Platform = "linux/arm64" }, func(c *CandidateBundle) { c.Resources = json.RawMessage(`{}`) }} {
		bad := proposed
		mutate(&bad)
		data, _ := json.Marshal(bad)
		if _, err := ParseCandidate(data); err == nil {
			t.Fatal("invalid candidate accepted")
		}
	}
	qualified := selected
	qualified.Version = 2
	qualified.Qualification.Commit = selected.SourceCommit
	qualified.Qualification.Suite = "pi-bundle/v1"
	data, _ = json.Marshal(qualified)
	if _, err := ParseBundle(data); err != nil {
		t.Fatal(err)
	}
	qualified.Qualification.Commit = ""
	data, _ = json.Marshal(qualified)
	if _, err := ParseBundle(data); err == nil {
		t.Fatal("missing actual qualification commit accepted")
	}
}
