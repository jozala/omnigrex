package github

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

var handoffLink = regexp.MustCompile(`\[([^\[\]\n]+)\]\((https?://[^\s()<>]+)\)`)

// safeHandoffMarkdown deliberately permits only lightweight Markdown. Escaping
// ampersands first prevents encoded HTML/schemes from gaining a second meaning.
// No fences, reference links, images, indentation blocks or raw HTML are owned
// by the agent; only this renderer owns expandable sections.
func safeHandoffMarkdown(value, credential string) string {
	if credential != "" {
		value = strings.ReplaceAll(value, credential, "[REDACTED]")
	}
	value = workflow.CleanHandoffText(value)
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, " \t")
	}
	value = strings.Join(lines, "\n")
	escape := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\\", "\\\\", "[", "\\[", "]", "\\]", "!", "\\!", "#", "\\#", "|", "\\|", "~", "\\~")
		s = r.Replace(s)
		// Keep inline code, but no multiline or unbalanced code spans.
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			if strings.Contains(line, "``") || strings.Count(line, "`")%2 != 0 {
				line = strings.ReplaceAll(line, "`", "\\`")
			}
			if strings.Trim(line, "- ") == "" {
				line = strings.ReplaceAll(line, "-", "\\-")
			}
			lines[i] = line
		}
		return strings.Join(lines, "\n")
	}
	var out strings.Builder
	cursor := 0
	for _, m := range handoffLink.FindAllStringSubmatchIndex(value, -1) {
		out.WriteString(escape(value[cursor:m[0]]))
		target := value[m[4]:m[5]]
		parsed, err := url.Parse(target)
		if err != nil || parsed.Host == "" || parsed.User != nil || m[0] > 0 && value[m[0]-1] == '!' {
			out.WriteString(escape(value[m[0]:m[1]]))
		} else {
			// URL punctuation is percent-encoded where it could acquire Markdown meaning.
			target = strings.NewReplacer("&", "&amp;", "\\", "%5C", "`", "%60", "\"", "%22").Replace(target)
			out.WriteString("[" + escape(value[m[2]:m[3]]) + "](" + target + ")")
		}
		cursor = m[1]
	}
	out.WriteString(escape(value[cursor:]))
	return out.String()
}

func handoffCommentBody(effect store.WorkflowGitHubEffectContext, credential string) string {
	body := "## Human action needed\n\n"
	if effect.SafetyDiagnostic {
		body += "An operational safety condition needs human attention. This report does not establish that the Workflow is stopped or eligible for reactivation."
	} else {
		switch workflow.Reason(effect.HandoffReason) {
		case workflow.ReasonAgentBlocked:
			body += "Autonomous work stopped because the agent reported a blocker."
		case workflow.ReasonReviewBudgetExhausted:
			body += "Autonomous work stopped after the review budget was exhausted. Human judgment is needed before another Attempt."
		case workflow.Reason("ci_evidence_incomplete"):
			body += "Autonomous work stopped because required CI evidence was still missing, pending, or unavailable at the deadline. This does not establish that the code failed."
		case workflow.Reason("ci_repair_budget_exhausted"):
			body += "Autonomous work stopped after the automatic CI-repair budget was exhausted. A maintainer must inspect the remaining failures before another Attempt."
		case workflow.ReasonAgentTurnPreparationFailed, workflow.ReasonAssignmentConfigurationConflict:
			body += "Autonomous work stopped because the required agent configuration or execution prerequisites could not be prepared."
		case workflow.ReasonAgentTurnMutationReconciliationExhausted, workflow.ReasonTerminalCorroborationExhausted:
			body += "Autonomous work stopped because the outcome of a recorded operation could not be safely verified. Resolve the recorded operation rather than repeating its effect manually."
		case workflow.ReasonTerminalRevalidationHumanControl:
			body += "Autonomous work stopped because the Agent Session is human-controlled. Ask the operator to transfer control to automation before reactivation."
		case workflow.ReasonInfrastructureRetriesExhausted:
			body += "Autonomous work stopped after infrastructure retries were exhausted."
		default:
			body += "Autonomous work stopped because a required prerequisite or safe recovery could not be established."
		}
	}
	section := func(title, text string) {
		if strings.TrimSpace(text) != "" {
			body += "\n\n### " + title + "\n\n" + safeHandoffMarkdown(text, credential)
		}
	}
	c := effect.HandoffContext
	if c != nil {
		section("Reported explanation", c.Explanation)
		section("Reported observations", c.Observations)
		section("Next action (agent proposal)", c.Intervention)
	}
	if c == nil || strings.TrimSpace(c.Intervention) == "" {
		body += "\n\n### Next action\n\nInspect the reported condition with the [repository owner](https://github.com/jozala/omnigrex/blob/main/docs/repository-owners/troubleshooting.md) or [operator](https://github.com/jozala/omnigrex/blob/main/docs/operators/troubleshooting.md) before retrying."
	}
	body += fmt.Sprintf("\n\nWork Item: #%d.", effect.IssueNumber)
	if p := effect.ChangeProposal; p != nil {
		body += fmt.Sprintf(" Current Change Proposal: #%d, tracked head `%s`.", p.Number, sanitizeHandoffText(p.HeadSHA, credential, 80))
	}
	if effect.HandoffSourceHeadSHA != "" {
		body += "\n\nSource observation head: `" + sanitizeHandoffText(effect.HandoffSourceHeadSHA, credential, 80) + "`. Reported work and evidence refer to that observation, not qualification of a later tracked head."
	}
	body += "\n\n### Continuation\n\n"
	// Normal publication is revision-fenced to NEEDS_HUMAN; closed/closing
	// Workflows cannot enter here as normal publications.
	if !effect.SafetyDiagnostic && effect.State == workflow.StateNeedsHuman {
		body += "After resolving the cause, confirm the Issue is open and re-add `omnigrex:run` to the Issue"
		if effect.ChangeProposal != nil {
			body += " or durable current Change Proposal"
		}
		body += ". Successful CI, comments, and pushes alone do not resume the Workflow. Reactivation creates a fresh Workflow Attempt; prerequisites are revalidated."
	} else {
		body += "Use the [operator recovery guide](https://github.com/jozala/omnigrex/blob/main/docs/operators/troubleshooting.md) to establish the supported recovery procedure. Do not infer reactivation eligibility from this safety report."
	}
	if c != nil {
		section("Completed work (agent report)", c.CompletedWork)
		section("Material uncertainty (agent report)", c.Uncertainty)
	}
	body += "\n\n<details>\n<summary>Supporting evidence and technical diagnostics</summary>\n\n"
	if c != nil && c.Evidence != "" {
		body += safeHandoffMarkdown(c.Evidence, credential) + "\n\n"
	}
	body += "Reason: " + safeHandoffMarkdown(sanitizeHandoffText(effect.HandoffReason, credential, 160), credential) + ".\n\n"
	diagnostic := sanitizeHandoffText(effect.HandoffDiagnostic, credential, 0)
	// Legacy diagnostics were not intake bounded. Do not imply the entire source
	// was published when retaining only a safe bounded excerpt.
	if len([]rune(diagnostic)) > 4096 {
		diagnostic = string([]rune(diagnostic)[:4096]) + " [Diagnostic excerpt; remainder omitted.]"
	}
	renderedDiagnostic := safeHandoffMarkdown(diagnostic, credential)
	if len(renderedDiagnostic) > 12000 {
		end := 12000
		for !utf8.RuneStart(renderedDiagnostic[end]) {
			end--
		}
		renderedDiagnostic = renderedDiagnostic[:end] + " [Diagnostic excerpt; remainder omitted.]"
	}
	body += renderedDiagnostic
	return body + "\n\n</details>"
}
