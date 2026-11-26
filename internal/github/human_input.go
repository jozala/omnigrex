package github

import (
	"context"
	"github.com/jozala/omnigrex/internal/store"
)

// WaitingForInput is an Issue-only observation, outside the exclusive Stage set.
const WaitingForInput = "omnigrex:waiting-for-input"

var humanInputLabel = Label{Name: WaitingForInput, Color: "fbca04", Description: "Awaiting a human clarification response or exact-plan confirmation"}

func (worker *LabelWorker) reconcileHumanInput(ctx context.Context, token string, effect store.WorkflowGitHubEffectContext) error {
	target, err := positiveGitHubNumber(effect.IssueNumber)
	if err != nil {
		return err
	}
	if effect.HumanInputWaiting {
		labels, err := worker.api.ListRepositoryLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName)
		if err != nil {
			return err
		}
		if !labelPresent(labels, WaitingForInput) {
			if _, err := worker.api.CreateRepositoryLabel(ctx, token, effect.RepositoryOwner, effect.RepositoryName, humanInputLabel); err != nil && !isAPIStatus(err, 422) {
				return err
			}
		}
	}
	labels, err := worker.api.ListIssueLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target)
	if err != nil {
		return err
	}
	if effect.HumanInputWaiting && !labelPresent(labels, WaitingForInput) {
		if _, err = worker.api.AddIssueLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target, []string{WaitingForInput}); err != nil {
			return err
		}
	} else if !effect.HumanInputWaiting && labelPresent(labels, WaitingForInput) {
		if err = worker.api.RemoveIssueLabel(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target, WaitingForInput); err != nil && !isAPIStatus(err, 404) {
			return err
		}
	}
	final, err := worker.api.ListIssueLabels(ctx, token, effect.RepositoryOwner, effect.RepositoryName, target)
	if err != nil {
		return err
	}
	if labelPresent(final, WaitingForInput) != effect.HumanInputWaiting {
		return ErrLabelReconciliation
	}
	return nil
}
