package github_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestManagedLabelsAreStable(t *testing.T) {
	want := []githubapi.Label{
		{Name: "omnigrex:run", Color: "1f6feb", Description: "Start or resume Omnigrex work"},
		{Name: "omnigrex:developing", Color: "d4a72c", Description: "Omnigrex Developer is working"},
		{Name: "omnigrex:reviewing", Color: "8250df", Description: "Omnigrex Reviewer is reviewing"},
		{Name: "omnigrex:pr-ready", Color: "2da44e", Description: "Change Proposal is ready for human review"},
		{Name: "omnigrex:needs-human", Color: "cf222e", Description: "Omnigrex needs human attention"},
	}
	got := githubapi.ManagedLabels()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ManagedLabels() = %#v, want %#v", got, want)
	}
	got[0].Name = "mutated"
	if githubapi.ManagedLabels()[0].Name != "omnigrex:run" {
		t.Error("ManagedLabels() returned mutable package state")
	}
}

func TestLabelReconcilerEnsuresMissingLabelsAndConvergesDesiredState(t *testing.T) {
	labels := &fakeLabelAPI{
		repositoryLabels: []githubapi.Label{
			{Name: "external"},
			{Name: string(githubapi.StateReviewing)},
		},
		issueLabels: []githubapi.Label{
			{Name: string(githubapi.StateRun)},
			{Name: "priority:high"},
			{Name: string(githubapi.StatePRReady)},
			{Name: string(githubapi.StateReviewing)},
		},
	}
	reconciler := githubapi.NewLabelReconciler(labels)

	if err := reconciler.EnsureManagedLabels(context.Background(), "installation-token", "acme", "widgets"); err != nil {
		t.Fatalf("EnsureManagedLabels() error = %v", err)
	}
	wantCreated := []string{
		"omnigrex:run",
		"omnigrex:developing",
		"omnigrex:pr-ready",
		"omnigrex:needs-human",
	}
	if fmt.Sprint(labelNames(labels.created)) != fmt.Sprint(wantCreated) {
		t.Errorf("created labels = %v, want %v", labelNames(labels.created), wantCreated)
	}
	if err := reconciler.EnsureManagedLabels(context.Background(), "installation-token", "acme", "widgets"); err != nil {
		t.Fatalf("second EnsureManagedLabels() error = %v", err)
	}
	if len(labels.created) != len(wantCreated) {
		t.Errorf("second ensure created more labels: %v", labelNames(labels.created))
	}

	if err := reconciler.ReconcileState(context.Background(), "installation-token", "acme", "widgets", 17, githubapi.StateReviewing); err != nil {
		t.Fatalf("ReconcileState() error = %v", err)
	}
	wantIssueLabels := []string{"priority:high", "omnigrex:reviewing"}
	if fmt.Sprint(labelNames(labels.issueLabels)) != fmt.Sprint(wantIssueLabels) {
		t.Errorf("reconciled labels = %v, want %v", labelNames(labels.issueLabels), wantIssueLabels)
	}
	if labels.replacements != 0 || len(labels.additions) != 0 || fmt.Sprint(labels.removals) != "[omnigrex:run omnigrex:pr-ready]" {
		t.Errorf("label mutations = replacements %d, additions %v, removals %v", labels.replacements, labels.additions, labels.removals)
	}
	if err := reconciler.ReconcileState(context.Background(), "installation-token", "acme", "widgets", 17, githubapi.StateReviewing); err != nil {
		t.Fatalf("second ReconcileState() error = %v", err)
	}
	if labels.replacements != 0 || len(labels.additions) != 0 || len(labels.removals) != 2 {
		t.Errorf("converged reconciliation made extra mutations: replacements %d, additions %v, removals %v", labels.replacements, labels.additions, labels.removals)
	}
}

func TestLabelReconcilerPreservesLabelAddedConcurrentlyByHuman(t *testing.T) {
	labels := &fakeLabelAPI{
		issueLabels:     []githubapi.Label{{Name: string(githubapi.StateDeveloping)}},
		concurrentLabel: "security-review",
	}
	reconciler := githubapi.NewLabelReconciler(labels)
	if err := reconciler.ReconcileState(context.Background(), "installation-token", "acme", "widgets", 17, githubapi.StateReviewing); err != nil {
		t.Fatalf("ReconcileState() error = %v", err)
	}
	want := []string{"omnigrex:reviewing", "security-review"}
	if fmt.Sprint(labelNames(labels.issueLabels)) != fmt.Sprint(want) {
		t.Errorf("labels after concurrent human update = %v, want %v", labelNames(labels.issueLabels), want)
	}
}

func TestLabelReconcilerClearsManagedLabelsAndPreservesHumanLabels(t *testing.T) {
	labels := &fakeLabelAPI{issueLabels: []githubapi.Label{
		{Name: string(githubapi.StateRun)},
		{Name: "security-review"},
		{Name: string(githubapi.StatePRReady)},
	}}

	if err := githubapi.NewLabelReconciler(labels).ReconcileState(context.Background(), "installation-token", "acme", "widgets", 17, githubapi.StateNone); err != nil {
		t.Fatalf("ReconcileState(StateNone) error = %v", err)
	}
	if got := fmt.Sprint(labelNames(labels.issueLabels)); got != "[security-review]" {
		t.Errorf("labels after clear = %s, want [security-review]", got)
	}
	if len(labels.created) != 0 {
		t.Errorf("clear created repository labels: %v", labelNames(labels.created))
	}
}

func TestLabelReconcilerRejectsUnknownManagedState(t *testing.T) {
	reconciler := githubapi.NewLabelReconciler(&fakeLabelAPI{})
	err := reconciler.ReconcileState(context.Background(), "installation-token", "acme", "widgets", 17, githubapi.WorkflowState("omnigrex:unknown"))
	if !errors.Is(err, githubapi.ErrInvalidWorkflowState) {
		t.Errorf("ReconcileState() error = %v, want ErrInvalidWorkflowState", err)
	}
}

func TestLabelReconcilerAcceptsConcurrentLabelCreation(t *testing.T) {
	labels := &fakeLabelAPI{createConflictOnce: true}
	reconciler := githubapi.NewLabelReconciler(labels)
	if err := reconciler.EnsureManagedLabels(context.Background(), "installation-token", "acme", "widgets"); err != nil {
		t.Fatalf("EnsureManagedLabels() concurrent creation error = %v", err)
	}
	if len(labels.repositoryLabels) != len(githubapi.ManagedLabels()) {
		t.Errorf("repository labels = %v, want all managed labels", labelNames(labels.repositoryLabels))
	}
}

func TestLabelReconcilerDoesNotSuppressUnverifiedMutationResponses(t *testing.T) {
	t.Run("add validation failure", func(t *testing.T) {
		labels := &fakeLabelAPI{
			issueLabels: []githubapi.Label{{Name: string(githubapi.StateDeveloping)}},
			addErr:      &githubapi.APIError{StatusCode: 422, Method: "POST", Path: "/labels", Message: "validation failed"},
		}
		err := githubapi.NewLabelReconciler(labels).ReconcileState(context.Background(), "token", "acme", "widgets", 17, githubapi.StateReviewing)
		if err == nil {
			t.Fatal("ReconcileState() error = nil, want unverified 422")
		}
		if !containsLabel(labels.issueLabels, string(githubapi.StateDeveloping)) {
			t.Error("ReconcileState() removed the old state after desired-label addition failed")
		}
	})

	t.Run("remove not found while stale label remains", func(t *testing.T) {
		labels := &fakeLabelAPI{
			issueLabels: []githubapi.Label{{Name: string(githubapi.StateReviewing)}, {Name: string(githubapi.StateDeveloping)}},
			removeErr:   &githubapi.APIError{StatusCode: 404, Method: "DELETE", Path: "/labels/developing", Message: "not found"},
		}
		err := githubapi.NewLabelReconciler(labels).ReconcileState(context.Background(), "token", "acme", "widgets", 17, githubapi.StateReviewing)
		if err == nil {
			t.Fatal("ReconcileState() error = nil, want unverified 404")
		}
	})
}

type fakeLabelAPI struct {
	repositoryLabels   []githubapi.Label
	issueLabels        []githubapi.Label
	created            []githubapi.Label
	replacements       int
	additions          []string
	removals           []string
	concurrentLabel    string
	concurrentAdded    bool
	createConflictOnce bool
	addErr             error
	removeErr          error
}

func (api *fakeLabelAPI) ListRepositoryLabels(context.Context, string, string, string) ([]githubapi.Label, error) {
	return append([]githubapi.Label(nil), api.repositoryLabels...), nil
}

func (api *fakeLabelAPI) CreateRepositoryLabel(_ context.Context, _, _, _ string, label githubapi.Label) (githubapi.Label, error) {
	api.created = append(api.created, label)
	api.repositoryLabels = append(api.repositoryLabels, label)
	if api.createConflictOnce {
		api.createConflictOnce = false
		return githubapi.Label{}, &githubapi.APIError{StatusCode: 422, Method: "POST", Path: "/labels", Message: "already exists"}
	}
	return label, nil
}

func (api *fakeLabelAPI) ListIssueLabels(context.Context, string, string, string, int) ([]githubapi.Label, error) {
	return append([]githubapi.Label(nil), api.issueLabels...), nil
}

func (api *fakeLabelAPI) ReplaceIssueLabels(_ context.Context, _, _, _ string, _ int, names []string) ([]githubapi.Label, error) {
	api.replacements++
	api.issueLabels = make([]githubapi.Label, len(names))
	for index, name := range names {
		api.issueLabels[index] = githubapi.Label{Name: name}
	}
	return append([]githubapi.Label(nil), api.issueLabels...), nil
}

func (api *fakeLabelAPI) AddIssueLabels(_ context.Context, _, _, _ string, _ int, names []string) ([]githubapi.Label, error) {
	api.additions = append(api.additions, names...)
	if api.addErr != nil {
		return nil, api.addErr
	}
	for _, name := range names {
		if !containsLabel(api.issueLabels, name) {
			api.issueLabels = append(api.issueLabels, githubapi.Label{Name: name})
		}
	}
	return append([]githubapi.Label(nil), api.issueLabels...), nil
}

func (api *fakeLabelAPI) RemoveIssueLabel(_ context.Context, _, _, _ string, _ int, name string) error {
	api.removals = append(api.removals, name)
	if api.removeErr != nil {
		return api.removeErr
	}
	if api.concurrentLabel != "" && !api.concurrentAdded {
		api.issueLabels = append(api.issueLabels, githubapi.Label{Name: api.concurrentLabel})
		api.concurrentAdded = true
	}
	for index, label := range api.issueLabels {
		if label.Name == name {
			api.issueLabels = append(api.issueLabels[:index], api.issueLabels[index+1:]...)
			break
		}
	}
	return nil
}

func containsLabel(labels []githubapi.Label, name string) bool {
	for _, label := range labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func labelNames(labels []githubapi.Label) []string {
	names := make([]string, len(labels))
	for index, label := range labels {
		names[index] = label.Name
	}
	return names
}
