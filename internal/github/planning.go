package github

import (
	"fmt"
	"github.com/jozala/omnigrex/internal/collaboration"
	"strings"
)

func planningCheckpointBody(r collaboration.PlanningRevision) string {
	var b strings.Builder
	c := r.Content
	fmt.Fprintf(&b, "## %s\n\n**Planning revision `%s` — %s**\n\n### What changed\n\n%s\n\n", c.Title, r.ID, c.Status, c.Changes)
	if r.Supersedes != "" {
		fmt.Fprintf(&b, "Supersedes complete brief/plan pair `%s`; history remains accessible.\n\n", r.Supersedes)
	}
	fmt.Fprintf(&b, "### Requirements brief\n\n%s\n\n### Technical plan\n\n%s\n\n### Next action\n\n", c.RequirementsBrief, c.TechnicalPlan)
	if c.Status == "proposed" {
		fmt.Fprintf(&b, "Confirm shared understanding with `/omnigrex confirm-plan %s` on the first nonblank line of a new Issue comment. Repository write-or-higher permission is required. This queues final Planner validation, not implementation. Use `/omnigrex steer` to change direction.\n\n", r.ID)
	} else {
		fmt.Fprint(&b, "This is an incomplete draft, not a confirmation request. Continue through the current questions or `/omnigrex steer`.\n\n")
	}
	fmt.Fprintf(&b, "<details>\n<summary>Evidence and history</summary>\n\nInvestigated commit: `%s`.\nProcedure: `%s`.\n\nRepository evidence:\n", c.InvestigatedCommit, r.ProcedureVersion)
	for _, ref := range c.Evidence {
		fmt.Fprintf(&b, "- %s\n", ref)
	}
	fmt.Fprintf(&b, "\nOriginal request (%s; source %s):\n\n**%s**\n\n%s\n", r.OriginalRequest.Source.URL, r.OriginalRequest.Source.ID, r.OriginalRequest.Title, r.OriginalRequest.Body)
	for _, in := range r.Decisions {
		fmt.Fprintf(&b, "\nAccepted input `%s` by %s (%s), source %s (%s):\n\n%s\n", in.ID, in.Actor.Name, in.Actor.ID, in.Source.ID, in.Source.URL, in.Body)
	}
	fmt.Fprint(&b, "\nConfirmed decisions, assumptions and unresolved questions remain distinct. Structural presence checks do not establish semantic completeness or design correctness.\n\n</details>\n")
	return b.String()
}
