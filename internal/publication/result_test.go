package publication_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/publication"
)

func TestOpenResultRequiresCompleteVersionedEvidence(t *testing.T) {
	result := publication.OpenResult{Version: 1, PullRequestID: 654, NodeID: "PR_654", Number: 23, HTMLURL: "https://github.test/owner/repo/pull/23",
		HeadSHA: strings.Repeat("1", 40), RepositoryID: 116, RepositoryOwner: "owner", RepositoryName: "repo", BaseRef: "main", BaseSHA: strings.Repeat("2", 40), HeadRef: "omnigrex/issue-116", Status: "OPEN"}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	got, err := publication.DecodeOpenResult(raw)
	if err != nil || got != result {
		t.Fatalf("complete evidence = %#v, %v", got, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"old incomplete format", func(m map[string]any) { delete(m, "version"); delete(m, "base_sha"); delete(m, "repository_id") }},
		{"unknown version", func(m map[string]any) { m["version"] = 2 }},
		{"missing base", func(m map[string]any) { delete(m, "base_sha") }},
		{"unexpected field", func(m map[string]any) { m["trusted"] = true }},
		{"invalid repository", func(m map[string]any) { m["repository_id"] = 0 }},
		{"invalid head", func(m map[string]any) { m["head_sha"] = "short" }},
		{"invalid PR", func(m map[string]any) { m["pull_request_id"] = 0 }},
		{"unbounded metadata", func(m map[string]any) { m["node_id"] = strings.Repeat("x", 257) }},
		{"unknown lifecycle", func(m map[string]any) { m["status"] = "UNKNOWN" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var object map[string]any
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			test.mutate(object)
			bad, _ := json.Marshal(object)
			if _, err := publication.DecodeOpenResult(bad); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	for _, bad := range [][]byte{nil, []byte(`null`), append(append([]byte(nil), raw...), []byte(` {}`)...), []byte(strings.Repeat(" ", 8193))} {
		if _, err := publication.DecodeOpenResult(bad); err == nil {
			t.Fatal("non-object, trailing or oversized evidence accepted")
		}
	}
}
