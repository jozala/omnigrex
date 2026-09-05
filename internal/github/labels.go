package github

import (
	"context"
	"errors"
	"fmt"
)

type WorkflowState string

const (
	StateNone       WorkflowState = ""
	StateRun        WorkflowState = "omnigrex:run"
	StateDeveloping WorkflowState = "omnigrex:developing"
	StateReviewing  WorkflowState = "omnigrex:reviewing"
	StatePRReady    WorkflowState = "omnigrex:pr-ready"
	StateNeedsHuman WorkflowState = "omnigrex:needs-human"
)

var ErrInvalidWorkflowState = errors.New("invalid Omnigrex workflow state")
var ErrLabelReconciliation = errors.New("GitHub labels did not converge")

var managedLabels = [...]Label{
	{Name: string(StateRun), Color: "1f6feb", Description: "Start or resume Omnigrex work"},
	{Name: string(StateDeveloping), Color: "d4a72c", Description: "Omnigrex Developer is working"},
	{Name: string(StateReviewing), Color: "8250df", Description: "Omnigrex Reviewer is reviewing"},
	{Name: string(StatePRReady), Color: "2da44e", Description: "Change Proposal is ready for human review"},
	{Name: string(StateNeedsHuman), Color: "cf222e", Description: "Omnigrex needs human attention"},
}

func ManagedLabels() []Label {
	return append([]Label(nil), managedLabels[:]...)
}

type LabelAPI interface {
	ListRepositoryLabels(context.Context, string, string, string) ([]Label, error)
	CreateRepositoryLabel(context.Context, string, string, string, Label) (Label, error)
	ListIssueLabels(context.Context, string, string, string, int) ([]Label, error)
	ReplaceIssueLabels(context.Context, string, string, string, int, []string) ([]Label, error)
	AddIssueLabels(context.Context, string, string, string, int, []string) ([]Label, error)
	RemoveIssueLabel(context.Context, string, string, string, int, string) error
}

type LabelReconciler struct {
	api LabelAPI
}

func NewLabelReconciler(api LabelAPI) *LabelReconciler {
	return &LabelReconciler{api: api}
}

func (reconciler *LabelReconciler) EnsureManagedLabels(ctx context.Context, installationToken, owner, repository string) error {
	existing, err := reconciler.api.ListRepositoryLabels(ctx, installationToken, owner, repository)
	if err != nil {
		return err
	}
	existingNames := make(map[string]struct{}, len(existing))
	for _, label := range existing {
		existingNames[label.Name] = struct{}{}
	}
	for _, label := range managedLabels {
		if _, exists := existingNames[label.Name]; exists {
			continue
		}
		if _, err := reconciler.api.CreateRepositoryLabel(ctx, installationToken, owner, repository, label); err != nil {
			if !isAPIStatus(err, 422) {
				return err
			}
			refreshed, listErr := reconciler.api.ListRepositoryLabels(ctx, installationToken, owner, repository)
			if listErr != nil || !labelPresent(refreshed, label.Name) {
				return errors.Join(err, listErr)
			}
		}
	}
	return nil
}

func (reconciler *LabelReconciler) ReconcileState(ctx context.Context, installationToken, owner, repository string, issueNumber int, desired WorkflowState) error {
	if desired != StateNone && !isManagedState(desired) {
		return &ConfigurationError{Cause: fmt.Errorf("%w: %q", ErrInvalidWorkflowState, desired)}
	}
	if desired != StateNone {
		if err := reconciler.EnsureManagedLabels(ctx, installationToken, owner, repository); err != nil {
			return err
		}
	}
	current, err := reconciler.api.ListIssueLabels(ctx, installationToken, owner, repository, issueNumber)
	if err != nil {
		return err
	}
	managed := make(map[string]struct{}, len(managedLabels))
	for _, label := range managedLabels {
		managed[label.Name] = struct{}{}
	}
	desiredPresent := desired == StateNone
	for _, label := range current {
		if label.Name == string(desired) {
			desiredPresent = true
		}
	}
	if !desiredPresent {
		if _, err := reconciler.api.AddIssueLabels(ctx, installationToken, owner, repository, issueNumber, []string{string(desired)}); err != nil {
			if !isAPIStatus(err, 422) {
				return err
			}
			observed, observeErr := reconciler.api.ListIssueLabels(ctx, installationToken, owner, repository, issueNumber)
			if observeErr != nil || !labelPresent(observed, string(desired)) {
				return errors.Join(err, observeErr, ErrLabelReconciliation)
			}
		}
	}
	for _, label := range current {
		if _, isManaged := managed[label.Name]; !isManaged || desired != StateNone && label.Name == string(desired) {
			continue
		}
		if err := reconciler.api.RemoveIssueLabel(ctx, installationToken, owner, repository, issueNumber, label.Name); err != nil {
			if !isAPIStatus(err, 404) {
				return err
			}
			observed, observeErr := reconciler.api.ListIssueLabels(ctx, installationToken, owner, repository, issueNumber)
			if observeErr != nil || labelPresent(observed, label.Name) {
				return errors.Join(err, observeErr, ErrLabelReconciliation)
			}
		}
	}
	observed, err := reconciler.api.ListIssueLabels(ctx, installationToken, owner, repository, issueNumber)
	if err != nil {
		return err
	}
	if !labelsConverged(observed, desired) {
		return ErrLabelReconciliation
	}
	return nil
}

func isManagedState(state WorkflowState) bool {
	for _, label := range managedLabels {
		if label.Name == string(state) {
			return true
		}
	}
	return false
}

func labelPresent(labels []Label, name string) bool {
	for _, label := range labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func isAPIStatus(err error, status int) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.StatusCode == status
}

func labelsConverged(labels []Label, desired WorkflowState) bool {
	managedCount := 0
	for _, label := range labels {
		if isManagedState(WorkflowState(label.Name)) {
			managedCount++
			if label.Name != string(desired) {
				return false
			}
		}
	}
	if desired == StateNone {
		return managedCount == 0
	}
	return managedCount == 1
}
