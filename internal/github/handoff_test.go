package github

import (
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestActionFirstHandoffKeepsActionsAndUncertaintyVisible(t *testing.T) {
	effect := store.WorkflowGitHubEffectContext{
		State: workflow.StateNeedsHuman, IssueNumber: 235, HandoffReason: "agent_blocked",
		HandoffSourceHeadSHA: "head-a", ChangeProposal: &store.WorkflowGitHubChangeProposal{Number: 226, HeadSHA: "head-b"},
		HandoffContext: &workflow.HandoffContext{
			Explanation:   "The fix is published. Qualification remains incomplete.",
			Observations:  "Ubuntu package downloads failed; connectivity is a reported diagnosis, not proof that no other failures exist.",
			Intervention:  "A maintainer should check runner connectivity, then rerun:\n\n- [Docker](https://example.test/docker)\n- [Linux](https://example.test/linux)",
			CompletedWork: "Implemented the fix.\n\nLocal unit tests passed.",
			Uncertainty:   "Required CI is not yet qualified.", Evidence: "Synthetic test output, not a CI success claim.",
		}, HandoffDiagnostic: "technical failure",
	}
	body := handoffCommentBody(effect, "")
	visible, evidence, found := strings.Cut(body, "<details>")
	if !found {
		t.Fatal("no owned evidence section")
	}
	for _, want := range []string{"fix is published", "maintainer", "- [Docker](https://example.test/docker)", "- [Linux]", "omnigrex:run", "Successful CI, comments, and pushes alone do not resume", "Required CI is not yet qualified", "head-a", "head-b", "#226", "#235", "reported diagnosis", "Implemented the fix.\n\nLocal unit tests passed."} {
		if !strings.Contains(visible, want) {
			t.Errorf("visible handoff missing %q: %s", want, visible)
		}
	}
	if strings.Contains(visible, "agent_blocked") || strings.Contains(visible, "Synthetic test output") || !strings.Contains(evidence, "Synthetic test output") || !strings.Contains(evidence, "technical failure") {
		t.Fatal(body)
	}
}

func TestHandoffSafeMarkdownAndEveryFieldRedaction(t *testing.T) {
	malicious := "paragraph α\n\n- **bold** and `code`\n</details><script>bad</script><!-- omnigrex:v1 -->\n![image](https://example.test/i) [bad](javascript:alert)\n[reference][evil]\n[evil]: javascript:bad\n```\n# Heading\n\tindented\n\u202eevil\x00 synthetic-secret"
	c := (&workflow.HandoffContext{Explanation: malicious, Intervention: malicious, CompletedWork: malicious, Observations: malicious, Uncertainty: malicious, Evidence: malicious})
	body := handoffCommentBody(store.WorkflowGitHubEffectContext{State: workflow.StateNeedsHuman, HandoffReason: "agent_blocked", HandoffContext: c}, "synthetic-secret")
	for _, unsafe := range []string{"<script>", "<!--", "![image]", "[bad](javascript:", "[evil]:", "\n```", "\n# Heading", "\n\t", "\u202e", "\x00", "synthetic-secret"} {
		if strings.Contains(body, unsafe) {
			t.Errorf("unsafe %q survived: %s", unsafe, body)
		}
	}
	if strings.Count(body, "</details>") != 1 || strings.Count(body, `\[REDACTED\]`) != 6 || !strings.Contains(body, "paragraph α\n\n- **bold** and `code`") {
		t.Fatal(body)
	}
}

func TestHandoffFallbackSafetyAndSize(t *testing.T) {
	for _, reason := range []string{"agent_blocked", "infrastructure_retries_exhausted", "review_budget_exhausted", "unknown_legacy_reason"} {
		effect := store.WorkflowGitHubEffectContext{State: workflow.StateNeedsHuman, HandoffReason: reason, HandoffDiagnostic: strings.Repeat("&<", 40000)}
		body := handoffCommentBody(effect, "")
		if len(body) > 60000 || !strings.Contains(body, "excerpt; remainder omitted") || !strings.Contains(body, "omnigrex:run") || !strings.Contains(body, "troubleshooting.md") {
			t.Fatal(body)
		}
		effect.SafetyDiagnostic = true
		body = handoffCommentBody(effect, "")
		if strings.Contains(body, "re-add `omnigrex:run`") || strings.Contains(body, "Autonomous work stopped") {
			t.Fatal(body)
		}
	}
}

func TestAcceptedContextFitsCompletePublication(t *testing.T) {
	c := &workflow.HandoffContext{Explanation: strings.Repeat("*", 2990), Intervention: strings.Repeat("`", 2800)}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	effect := store.WorkflowGitHubEffectContext{State: workflow.StateNeedsHuman, HandoffReason: "agent_blocked", HandoffContext: c, HandoffDiagnostic: strings.Repeat("&", 40000)}
	body := handoffCommentBody(effect, "")
	if len(body)+256 > 60000 || strings.Count(body, "</details>") != 1 || !strings.Contains(body, c.Explanation) {
		t.Fatalf("accepted report not publishable: %d bytes", len(body))
	}
}
