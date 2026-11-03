package mcp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestPublicationReadTargetValidatesExactEvidenceChain(t *testing.T) {
	const base = "0123456789abcdef0123456789abcdef01234567"
	const first = "1123456789abcdef0123456789abcdef01234567"
	const final = "2123456789abcdef0123456789abcdef01234567"
	scope := ToolScope{Role: workflow.RoleDeveloper, AgentTurnID: "turn", ExecutionEpoch: 2,
		Repository: RepositoryScope{ID: 123, Owner: "acme", Name: "widgets"}, Branch: "omnigrex/issue-115", DefaultBranch: "main", HeadSHA: base}
	publish := func(number int64, before, after string) store.MutationReservation {
		raw, _ := json.Marshal(map[string]any{"head": after, "branch": scope.Branch, "changed": before != after})
		return store.MutationReservation{ID: "publish", AgentTurnID: scope.AgentTurnID, ExecutionEpoch: 2, InvocationNumber: number,
			ToolName: ToolPublishChanges, State: store.MutationSucceeded, ExternalService: "git", ExternalResourceID: "123:" + scope.Branch,
			ExpectedSHA: before, ProposedSHA: after, HistoryPublication: true, Result: raw}
	}
	open := store.MutationReservation{ID: "open", AgentTurnID: scope.AgentTurnID, ExecutionEpoch: 2, InvocationNumber: 2,
		ToolName: ToolOpenPR, State: store.MutationSucceeded, ExternalService: "github", ExternalResourceID: "123:" + scope.Branch + ":main", ExpectedSHA: first,
		Result: json.RawMessage(`{"version":1,"repository_id":123,"repository_owner":"acme","repository_name":"widgets","base_ref":"main","base_sha":"0123456789abcdef0123456789abcdef01234567","head_ref":"omnigrex/issue-115","status":"OPEN","pull_request_id":456,"node_id":"PR_456","number":23,"html_url":"https://github.test/pull/23","head_sha":"` + first + `"}`)}
	backend := &ProductionBackend{}
	chain := []store.MutationReservation{publish(1, base, first), open, publish(3, first, final)}
	target, err := backend.resolvePublicationRead(scope, chain)
	if err != nil || target.HeadSHA != final || !reflect.DeepEqual(target.PullRequest, &PullRequestScope{ID: 456, Number: 23}) {
		t.Fatalf("target = %#v, %v", target, err)
	}
	if scope.HeadSHA != base || scope.PullRequest != nil {
		t.Fatal("modified starting authority")
	}
	tests := []struct {
		name   string
		mutate func(*ToolScope, []store.MutationReservation)
	}{
		{"another turn", func(_ *ToolScope, m []store.MutationReservation) { m[0].AgentTurnID = "other" }},
		{"another epoch", func(_ *ToolScope, m []store.MutationReservation) { m[0].ExecutionEpoch++ }},
		{"another repository", func(_ *ToolScope, m []store.MutationReservation) { m[0].ExternalResourceID = "999:" + scope.Branch }},
		{"another branch", func(_ *ToolScope, m []store.MutationReservation) {
			m[0].Result = json.RawMessage(`{"head":"` + first + `","branch":"other","changed":true}`)
		}},
		{"unrelated proposed head", func(_ *ToolScope, m []store.MutationReservation) { m[2].ProposedSHA = first }},
		{"missing captured head", func(_ *ToolScope, m []store.MutationReservation) { m[0].ProposedSHA = "" }},
		{"unsuccessful mutation", func(_ *ToolScope, m []store.MutationReservation) { m[0].State = store.MutationUnknown }},
		{"out of order", func(_ *ToolScope, m []store.MutationReservation) { m[1].InvocationNumber = 1 }},
		{"unrelated PR", func(s *ToolScope, _ []store.MutationReservation) {
			s.PullRequest = &PullRequestScope{ID: 999, Number: 99}
		}},
		{"unrelated PR head", func(_ *ToolScope, m []store.MutationReservation) {
			m[1].Result = json.RawMessage(`{"version":1,"repository_id":123,"repository_owner":"acme","repository_name":"widgets","base_ref":"main","base_sha":"0123456789abcdef0123456789abcdef01234567","head_ref":"omnigrex/issue-115","status":"OPEN","pull_request_id":456,"node_id":"PR_456","number":23,"html_url":"https://github.test/pull/23","head_sha":"` + base + `"}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyScope := cloneToolScope(scope)
			copyChain := append([]store.MutationReservation(nil), chain...)
			test.mutate(&copyScope, copyChain)
			if target, err := backend.resolvePublicationRead(copyScope, copyChain); err == nil {
				t.Fatalf("accepted untrusted evidence: %#v", target)
			}
		})
	}
	// A retry can start at a verified, repaired PR head before it acknowledges
	// the publication that produced that head. The historical expected SHA is
	// not the starting authority, and the replay must not disable reads.
	repaired := cloneToolScope(scope)
	repaired.HeadSHA = first
	repaired.PullRequest = &PullRequestScope{ID: 456, Number: 23}
	for _, evidence := range [][]store.MutationReservation{chain[:1], {chain[0], chain[2]}} {
		want := first
		if len(evidence) > 1 {
			want = final
		}
		target, err := backend.resolvePublicationRead(repaired, evidence)
		if err != nil || target.HeadSHA != want || !reflect.DeepEqual(target.PullRequest, repaired.PullRequest) {
			t.Fatalf("repaired-head replay target = %#v, %v; want %s", target, err, want)
		}
	}
	// A retry repaired to the latest publication must remain readable even
	// immediately after replaying only the older publication in a longer chain.
	repaired.HeadSHA = final
	for _, evidence := range [][]store.MutationReservation{chain[:1], {chain[0], chain[2]}} {
		target, err := backend.resolvePublicationRead(repaired, evidence)
		if err != nil || target.HeadSHA != final || !reflect.DeepEqual(target.PullRequest, repaired.PullRequest) {
			t.Fatalf("historical replay changed or rejected target: %#v, %v", target, err)
		}
	}
	// Ignoring a nonconnecting publication never grants its result head.
	disconnected := append([]store.MutationReservation(nil), chain...)
	disconnected[2].ExpectedSHA = base
	target, err = backend.resolvePublicationRead(scope, disconnected)
	if err != nil || target.HeadSHA != first {
		t.Fatalf("disconnected publication granted a head: %#v, %v", target, err)
	}

	// Even already-applied results must retain complete publication validation.
	invalidApplied := chain[0]
	invalidApplied.ProposedSHA = final
	if _, err := backend.resolvePublicationRead(repaired, []store.MutationReservation{invalidApplied}); err == nil {
		t.Fatal("accepted invalid already-applied publication")
	}

	// An existing PR retains its identity while Developer publications advance only
	// the observation head. Reviewer reads stay frozen even in the presence of evidence.
	scope.PullRequest = &PullRequestScope{ID: 456, Number: 23}
	if target, err := backend.resolvePublicationRead(scope, chain); err != nil || target.HeadSHA != final {
		t.Fatalf("existing PR: %#v, %v", target, err)
	}
	scope.Role = workflow.RoleReviewer
	if target, err := backend.resolvePublicationRead(scope, chain); err != nil || target.HeadSHA != base || !reflect.DeepEqual(target.PullRequest, scope.PullRequest) {
		t.Fatalf("Reviewer: %#v, %v", target, err)
	}
}
