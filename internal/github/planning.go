package github

import (
	"fmt"
	"html"
	"strings"

	"github.com/jozala/omnigrex/internal/collaboration"
)

func planningCheckpointBody(r collaboration.PlanningRevision) string {
	var b strings.Builder
	c := r.Content
	fmt.Fprintf(&b, "## %s\n\n**%s · %s**\n\n%s\n\n### What changed\n\n%s\n\n", c.Title, r.Reference, c.Status, c.Summary, c.Changes)
	if r.SupersedesReference != "" {
		fmt.Fprintf(&b, "Supersedes complete brief/plan pair `%s`; history remains accessible.\n\n", r.SupersedesReference)
	}
	fmt.Fprint(&b, "### Unresolved decisions\n\n")
	if len(c.UnresolvedDecisions) == 0 {
		fmt.Fprint(&b, "None.\n\n")
	} else {
		for _, decision := range c.UnresolvedDecisions {
			fmt.Fprintf(&b, "- %s\n", decision)
		}
		fmt.Fprint(&b, "\n")
	}
	fmt.Fprint(&b, "### Next action\n\n")
	if c.Status == "proposed" {
		fmt.Fprintf(&b, "Review both complete documents below, then confirm shared understanding with `/omnigrex confirm-plan %s` on the first nonblank line of a new Issue comment. Repository write-or-higher permission is required. This queues final Planner validation, not implementation. Use `/omnigrex steer` to change direction.\n\n", r.Reference)
	} else {
		fmt.Fprint(&b, "This is an incomplete draft, not a confirmation request. Continue through the current questions or `/omnigrex steer`.\n\n")
	}
	fmt.Fprintf(&b, "<details>\n<summary>Complete requirements brief</summary>\n\n%s\n\n</details>\n\n<details>\n<summary>Complete technical plan</summary>\n\n%s\n\n</details>\n\n", c.RequirementsBrief, c.TechnicalPlan)
	fmt.Fprintf(&b, "<details>\n<summary>Evidence and history</summary>\n\nInvestigated commit: `%s`.\nProcedure: `%s`.\n\nRepository evidence:\n", c.InvestigatedCommit, r.ProcedureVersion)
	for _, ref := range c.Evidence {
		fmt.Fprintf(&b, "- %s\n", ref)
	}
	fmt.Fprintf(&b, "\nOriginal request: %s (source %s, %s).\n", provenanceText(r.OriginalRequest.Title), provenanceText(r.OriginalRequest.Source.ID), provenanceText(r.OriginalRequest.Source.URL))
	// The immutable record can be larger than a GitHub comment and can quote
	// operation markers. Publish references, never splice its body into output.
	const maxDecisionReferences = 10
	for i, in := range r.Decisions {
		if i == maxDecisionReferences {
			fmt.Fprintf(&b, "\n%d additional accepted inputs are retained in collaboration history.\n", len(r.Decisions)-i)
			break
		}
		fmt.Fprintf(&b, "\nInput %s by %s: %s.\n", provenanceText(in.Reference), provenanceText(in.Actor.Name), provenanceText(in.Source.URL))
	}
	fmt.Fprint(&b, "\nThis view contains bounded source references, not verbatim source contents. Complete original request and accepted inputs are preserved unchanged in collaboration history and available through `get_collaboration_record` (`planning_request` / `planning_history`); GitHub source links can change or disappear and do not replace the immutable internal record.\n")
	fmt.Fprint(&b, "\nConfirmed decisions, assumptions and unresolved questions remain distinct. Structural presence checks do not establish semantic completeness or design correctness.\n\n</details>\n")
	return b.String()
}

// Escape before presentation so quoted markers/Markdown cannot acquire authority.
// The omission indicator is explicit; the durable source is never shortened.
func provenanceText(value string) string {
	runes := []rune(value)
	if len(runes) > 160 {
		value = string(runes[:160]) + "… [reference shortened; full value in collaboration history]"
	}
	return "<code>" + html.EscapeString(value) + "</code>"
}
