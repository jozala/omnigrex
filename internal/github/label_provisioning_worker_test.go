package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type provisioningStore struct {
	lease      *store.LabelProvisioningLease
	claimOwner string
	completed  []store.LabelProvisioningLease
	failed     []provisioningFailure
	claimErr   error
}

type provisioningFailure struct {
	lease     store.LabelProvisioningLease
	cause     error
	retryable bool
}

func (durable *provisioningStore) ClaimLabelProvisioningJob(_ context.Context, owner string, _ time.Duration) (*store.LabelProvisioningLease, error) {
	durable.claimOwner = owner
	if durable.claimErr != nil {
		return nil, durable.claimErr
	}
	lease := durable.lease
	durable.lease = nil
	return lease, nil
}

func (durable *provisioningStore) HeartbeatLabelProvisioningJob(context.Context, store.LabelProvisioningLease, time.Duration) error {
	return nil
}

func (durable *provisioningStore) CompleteLabelProvisioningJob(_ context.Context, lease store.LabelProvisioningLease, _ json.RawMessage) error {
	durable.completed = append(durable.completed, lease)
	return nil
}

func (durable *provisioningStore) FailLabelProvisioningJob(_ context.Context, lease store.LabelProvisioningLease, cause error, retryable bool, _ time.Duration) error {
	durable.failed = append(durable.failed, provisioningFailure{lease: lease, cause: cause, retryable: retryable})
	return nil
}

type provisioningSigner struct{ token string }

func (signer *provisioningSigner) AppJWT(context.Context) (string, error) { return signer.token, nil }

type provisioningCredentials struct {
	token string
	err   error
}

func (provider *provisioningCredentials) RepositoryCredential(context.Context, string, string) (string, error) {
	return provider.token, provider.err
}

type provisioningAPI struct {
	githubapi.LabelAPI
	installationID int64
	resolveErr     error
	repository     githubapi.InstallationRepository
	repositoryErr  error
	listed         []githubapi.Label
	created        []string
	createErr      error
}

func (api *provisioningAPI) ResolveRepositoryInstallation(context.Context, string, string, string) (int64, error) {
	return api.installationID, api.resolveErr
}

func (api *provisioningAPI) GetRepository(context.Context, string, string, string) (githubapi.InstallationRepository, error) {
	return api.repository, api.repositoryErr
}

func (api *provisioningAPI) ListRepositoryLabels(context.Context, string, string, string) ([]githubapi.Label, error) {
	return api.listed, nil
}

func (api *provisioningAPI) CreateRepositoryLabel(_ context.Context, _, _, _ string, label githubapi.Label) (githubapi.Label, error) {
	if api.createErr != nil {
		return githubapi.Label{}, api.createErr
	}
	api.created = append(api.created, label.Name)
	return label, nil
}

func (api *provisioningAPI) ListIssueLabels(context.Context, string, string, string, int) ([]githubapi.Label, error) {
	return nil, nil
}

func (api *provisioningAPI) AddIssueLabels(context.Context, string, string, string, int, []string) ([]githubapi.Label, error) {
	return nil, nil
}

func (api *provisioningAPI) RemoveIssueLabel(context.Context, string, string, string, int, string) error {
	return nil
}

func provisioningLease() *store.LabelProvisioningLease {
	return &store.LabelProvisioningLease{
		LabelProvisioningJob: store.LabelProvisioningJob{
			ID: "40000000-0000-4000-8000-000000000001", RepositoryID: 9123,
			RepositoryOwner: "jozala", RepositoryName: "omnigrex",
			InstallationID: 99, SourceDeliveryID: "123e4567-e89b-12d3-a456-426614174000",
			LeaseToken: "50000000-0000-4000-8000-000000000001",
		},
		Attempt: 1,
	}
}

func provisioningWorker(durable *provisioningStore, api *provisioningAPI) *githubapi.LabelProvisioningWorker {
	worker, err := githubapi.NewLabelProvisioningWorker(durable, &provisioningSigner{token: "app-jwt"},
		&provisioningCredentials{token: "installation-token"}, api, githubapi.LabelProvisioningWorkerConfig{
			ClaimOwner: "provisioner", LeaseDuration: 30 * time.Second,
			HeartbeatInterval: 10 * time.Second, IdlePollInterval: time.Second, RetryDelay: 5 * time.Second,
		})
	if err != nil {
		panic(err)
	}
	return worker
}

func TestLabelProvisioningWorkerCreatesOnlyMissingLabels(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	api := &provisioningAPI{installationID: 99,
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		listed:     []githubapi.Label{{Name: "omnigrex:run", Color: "old", Description: "old"}},
	}
	worker := provisioningWorker(durable, api)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want success", processed, err)
	}
	if len(durable.completed) != 1 || len(durable.failed) != 0 {
		t.Fatalf("completed/failed = (%d, %d), want (1, 0)", len(durable.completed), len(durable.failed))
	}
	if len(api.created) != 4 {
		t.Errorf("created labels = %#v, want four missing labels", api.created)
	}
	for _, name := range api.created {
		if name == "omnigrex:run" {
			t.Errorf("existing omnigrex:run was recreated")
		}
	}
	if durable.claimOwner != "provisioner" {
		t.Errorf("claim owner = %q, want provisioner", durable.claimOwner)
	}
}

func TestLabelProvisioningWorkerFailsTerminallyOnLostAccess(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	api := &provisioningAPI{resolveErr: &githubapi.NotInstalledError{Owner: "jozala", Repository: "omnigrex"}}
	worker := provisioningWorker(durable, api)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want terminal failure", processed, err)
	}
	if len(durable.failed) != 1 || durable.failed[0].retryable {
		t.Errorf("failures = %#v, want one terminal failure", durable.failed)
	}
	if len(durable.completed) != 0 {
		t.Errorf("completed = %d, want 0", len(durable.completed))
	}
}

func TestLabelProvisioningWorkerFailsTerminallyOnIdentityMismatch(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	api := &provisioningAPI{installationID: 100,
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"}}
	worker := provisioningWorker(durable, api)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want mismatch failure", processed, err)
	}
	if len(durable.failed) != 1 || durable.failed[0].retryable {
		t.Errorf("failures = %#v, want one terminal mismatch failure", durable.failed)
	}
}

func TestLabelProvisioningWorkerRetriesTransientLabelFailure(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	api := &provisioningAPI{installationID: 99,
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		createErr:  &githubapi.TransientError{Cause: errors.New("unavailable")},
	}
	worker := provisioningWorker(durable, api)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want retryable failure", processed, err)
	}
	if len(durable.failed) != 1 || !durable.failed[0].retryable {
		t.Errorf("failures = %#v, want one retryable failure", durable.failed)
	}
}
