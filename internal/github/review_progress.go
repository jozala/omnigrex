package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const WaitingForCI = "omnigrex:waiting-for-ci"

var waitingLabel = Label{Name: WaitingForCI, Color: "8250df", Description: "Review Stage queued for controller CI observation; no Reviewer Turn running"}

type reviewProgressAPI interface {
	ListIssueComments(context.Context, string, string, string, int) ([]IssueComment, error)
	CreateIssueComment(context.Context, string, string, string, int, CommentRequest) (IssueComment, error)
	UpdateIssueComment(context.Context, string, string, string, int, int64, CommentRequest) (IssueComment, error)
	DeleteIssueComment(context.Context, string, string, string, int64) error
}

func (worker *LabelWorker) reconcileReviewProgress(ctx context.Context, token string, effect store.WorkflowGitHubEffectContext, targets []int) (map[int]int64, error) {
	progress := map[int]int64{}
	if effect.ReviewAdmission == nil && effect.ReviewPresentationRevision == 0 {
		return progress, nil
	}
	waiting := effect.State == workflow.StateReviewing && effect.ReviewAdmission != nil && effect.ReviewAdmission.Status == "WAITING"
	if waiting {
		labels, err := worker.api.ListRepositoryLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName)
		if err != nil {
			return nil, err
		}
		if !labelPresent(labels, WaitingForCI) {
			if _, err = worker.api.CreateRepositoryLabel(ctx, token, effect.RepositoryOwner, effect.RepositoryName, waitingLabel); err != nil && !isAPIStatus(err, 422) {
				return nil, err
			}
		}
	}
	for _, target := range targets {
		labels, err := worker.api.ListIssueLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target)
		if err != nil {
			return nil, err
		}
		if waiting && !labelPresent(labels, WaitingForCI) {
			if _, err = worker.api.AddIssueLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target, []string{WaitingForCI}); err != nil {
				return nil, err
			}
		}
		if !waiting && labelPresent(labels, WaitingForCI) {
			if err = worker.api.RemoveIssueLabel(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target, WaitingForCI); err != nil && !isAPIStatus(err, 404) {
				return nil, err
			}
		}
	}
	if effect.ReviewAdmission == nil {
		return progress, nil
	}
	api, ok := worker.api.(reviewProgressAPI)
	if !ok {
		return nil, errors.New("review progress comment API unavailable")
	}
	marker := Marker{WorkflowID: effect.WorkflowID, OperationID: effect.WorkflowID}
	rendered, err := RenderMarker(marker)
	if err != nil {
		return nil, err
	}
	body := reviewProgressBody(effect, waiting)
	for _, target := range targets {
		comments, err := api.ListIssueComments(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target)
		if err != nil {
			return nil, err
		}
		marked := exactMarkedComments(comments, marker)
		sort.Slice(marked, func(i, j int) bool { return marked[i].ID < marked[j].ID })
		if len(marked) == 0 {
			// A lost create response is reconciled on retry by the stable marker.
			if _, err = api.CreateIssueComment(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target, CommentRequest{Body: body, Marker: rendered}); err != nil {
				return nil, err
			}
		} else {
			if marked[0].Body != JoinBodyParts(body, rendered) {
				if _, err = api.UpdateIssueComment(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target, marked[0].ID, CommentRequest{Body: body, Marker: rendered}); err != nil {
					return nil, err
				}
			}
			for _, duplicate := range marked[1:] {
				if err = api.DeleteIssueComment(ctx, token, effect.RepositoryOwner, effect.RepositoryName, duplicate.ID); err != nil && !isAPIStatus(err, 404) {
					return nil, err
				}
			}
		}
		fresh, err := api.ListIssueComments(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target)
		if err != nil {
			return nil, err
		}
		final := exactMarkedComments(fresh, marker)
		if len(final) != 1 || final[0].Body != JoinBodyParts(body, rendered) {
			return nil, ErrHandoffCommentReconciliation
		}
		progress[target] = final[0].ID
	}
	return progress, nil
}

func reviewProgressBody(effect store.WorkflowGitHubEffectContext, waiting bool) string {
	r := effect.ReviewAdmission
	heading := "Review CI wait released; review queued"
	if waiting {
		heading = "Review queued: waiting for CI"
	} else if r.Status == "CANCELLED" || effect.State != workflow.StateReviewing {
		heading = "Review CI wait no longer pending"
	} else if r.Status == "ADMITTED" {
		heading = "Review CI wait released; Reviewer Turn allocated"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "**%s**\n\nPR: #%d; reviewed head: `%s`\n\nDeadline: %s\n", heading, r.PullRequestNumber, r.HeadSHA, r.Deadline.UTC().Format(time.RFC3339))
	if waiting {
		text.WriteString("\nNo Reviewer Agent Turn is running.\n")
	}
	var observation struct {
		Reason  string `json:"release_reason"`
		Targets []struct {
			Commit   string                `json:"commit"`
			Results  []CIRequirementResult `json:"results"`
			Failures []CIEvidenceFailure   `json:"failures"`
		} `json:"targets"`
		Inventory CIInventory         `json:"expected_ci"`
		Failures  []CIEvidenceFailure `json:"failures"`
	}
	_ = json.Unmarshal(r.Observation, &observation)
	if observation.Reason != "" {
		fmt.Fprintf(&text, "\nScheduling observation: `%s`.\n", observation.Reason)
	}
	count := 0
	for _, target := range observation.Targets {
		for _, result := range target.Results {
			if count >= 20 {
				break
			}
			count++
			fmt.Fprintf(&text, "- %s (%s, App %d): %s", safeProgressText(result.Requirement.Context), result.Source, result.Requirement.AppID, result.State)
			if result.Conclusion != "" {
				fmt.Fprintf(&text, " (%s)", safeProgressText(result.Conclusion))
			}
			text.WriteByte('\n')
		}
	}
	for _, rule := range observation.Inventory.Unresolved {
		if count >= 20 {
			break
		}
		count++
		fmt.Fprintf(&text, "- Unresolved requirement: %s\n", safeProgressText(rule.Type))
	}
	failures := append(append([]CIEvidenceFailure{}, observation.Failures...), observation.Inventory.Failures...)
	for _, target := range observation.Targets {
		failures = append(failures, target.Failures...)
	}
	for _, failure := range failures {
		if count >= 20 {
			break
		}
		count++
		fmt.Fprintf(&text, "- Unreadable evidence: %s (%s)\n", safeProgressText(failure.Operation), safeProgressText(failure.Code))
	}
	text.WriteString("\nThis is a point-in-time scheduling observation, not passing CI or approval evidence. Use the existing CI tools for detailed diagnostics.\n")
	return text.String()
}
func safeProgressText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < ' ' || r == '`' || r == '<' || r == '>' || r == '@' || r == '[' || r == ']' {
			return ' '
		}
		return r
	}, value)
	runes := []rune(value)
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return string(runes)
}
