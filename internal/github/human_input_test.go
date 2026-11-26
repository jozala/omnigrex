package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestHumanInputOverlayIsIssueOnlyAndIndependent(t *testing.T) {
	for _, mode := range []string{"add", "remove", "preparation", "stale-pr-ready", "upgrade"} {
		t.Run(mode, func(t *testing.T) {
			effect := testEffect(workflow.StateDeveloping, 3)
			effect.HumanInputWaiting = mode == "add"
			effect.VerificationPending = mode == "preparation"
			effect.HumanInputOnly = mode == "upgrade"
			if mode == "stale-pr-ready" {
				effect.State = workflow.StatePRReady
				effect.ReadyForSHA = testReadySHA
			}
			api := newFakeVisibleEffectAPI()
			api.issueLabels[17] = []Label{{Name: string(StateDeveloping)}, {Name: "user"}, {Name: WaitingForCI}}
			api.issueLabels[23] = []Label{{Name: string(StateDeveloping)}, {Name: "pr-user"}}
			if mode != "add" {
				api.issueLabels[17] = append(api.issueLabels[17], humanInputLabel)
			}
			api.pullRequest.Head.SHA = testNewSHA
			workerStore := newFakeVisibleEffectStore(store.ReconcileGitHubLabelsJobKind, effect)
			worker := newTestLabelWorker(t, workerStore, api, testWorkerConfig())
			_, err := worker.ProcessNext(context.Background())
			if mode == "stale-pr-ready" {
				if err == nil || !strings.Contains(err.Error(), ErrStalePullRequestHead.Error()) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if fakeHasLabel(api.issueLabels[17], WaitingForInput) != effect.HumanInputWaiting {
				t.Fatal(api.issueLabels[17])
			}
			if fakeHasLabel(api.issueLabels[23], WaitingForInput) {
				t.Fatal("overlay on Change Proposal")
			}
			if !fakeHasLabel(api.issueLabels[17], "user") || !fakeHasLabel(api.issueLabels[17], WaitingForCI) {
				t.Fatal("unrelated overlay or user label removed")
			}
			if mode == "preparation" || mode == "upgrade" {
				assertLabelNames(t, api.issueLabels[17], string(StateDeveloping), "user", WaitingForCI)
				assertLabelNames(t, api.issueLabels[23], string(StateDeveloping), "pr-user")
			}
			if mode == "add" {
				if !labelPresent(api.repositoryLabels, WaitingForInput) {
					t.Fatal("missing repository label")
				}
				count := len(api.repositoryLabels)
				if _, err := worker.reconcile(context.Background(), testCredential, effect, store.JobLease{}); err != nil {
					t.Fatal(err)
				}
				if len(api.repositoryLabels) != count {
					t.Fatal("non-idempotent repository label creation")
				}
			}
		})
	}
}

type nonConvergingInputAPI struct{ *fakeVisibleEffectAPI }

func (api nonConvergingInputAPI) AddIssueLabels(context.Context, string, string, string, int, []string) ([]Label, error) {
	return nil, nil
}
func TestHumanInputOverlayVerifiesFinalRead(t *testing.T) {
	api := nonConvergingInputAPI{newFakeVisibleEffectAPI()}
	effect := testEffect(workflow.StatePlanning, 3)
	effect.HumanInputWaiting = true
	worker := &LabelWorker{api: api}
	if err := worker.reconcileHumanInput(context.Background(), testCredential, effect); !errors.Is(err, ErrLabelReconciliation) {
		t.Fatal(err)
	}
}
