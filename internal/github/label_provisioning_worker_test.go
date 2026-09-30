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
	lease      *store.JobLease
	claimQueue string
	claimKind  string
	claimOwner string
	completed  []store.JobLease
	failed     []provisioningFailure
	claimErr   error
}

type provisioningFailure struct {
	lease     store.JobLease
	cause     error
	retryable bool
}

func (durable *provisioningStore) ClaimJobKind(_ context.Context, queue, kind, owner string, _ time.Duration) (*store.JobLease, error) {
	durable.claimQueue, durable.claimKind, durable.claimOwner = queue, kind, owner
	if durable.claimErr != nil {
		return nil, durable.claimErr
	}
	lease := durable.lease
	durable.lease = nil
	return lease, nil
}

func (durable *provisioningStore) HeartbeatJob(context.Context, store.JobLease, time.Duration) error {
	return nil
}

func (durable *provisioningStore) CompleteJob(_ context.Context, lease store.JobLease, _ json.RawMessage) error {
	durable.completed = append(durable.completed, lease)
	return nil
}

func (durable *provisioningStore) FailJob(_ context.Context, lease store.JobLease, cause error, retryable bool, _ time.Duration) error {
	durable.failed = append(durable.failed, provisioningFailure{lease: lease, cause: cause, retryable: retryable})
	return nil
}

type provisioningCredentials struct {
	token          string
	installationID int64
	err            error
}

func (provider *provisioningCredentials) InstallationCredential(_ context.Context, installationID int64) (string, error) {
	provider.installationID = installationID
	return provider.token, provider.err
}

type provisioningAPI struct {
	githubapi.LabelAPI
	listed        []githubapi.InstallationRepository
	listInvalid   []string
	listErr       error
	repository    githubapi.InstallationRepository
	repositoryErr error
	labels        []githubapi.Label
	created       []string
	createErr     error
}

func (api *provisioningAPI) ListInstallationRepositories(context.Context, string) ([]githubapi.InstallationRepository, []string, error) {
	return api.listed, api.listInvalid, api.listErr
}

func (api *provisioningAPI) GetRepository(context.Context, string, string, string) (githubapi.InstallationRepository, error) {
	return api.repository, api.repositoryErr
}

func (api *provisioningAPI) ListRepositoryLabels(context.Context, string, string, string) ([]githubapi.Label, error) {
	return api.labels, nil
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

func provisioningLease() *store.JobLease {
	payload, err := store.MarshalLabelProvisioningPayload(
		store.LabelProvisioningRepository{ID: 9123}, 99)
	if err != nil {
		panic(err)
	}
	return &store.JobLease{
		Job: store.Job{
			JobSpec: store.JobSpec{
				Queue: store.LabelProvisioningQueue, Kind: store.ProvisionManagedLabelsJobKind,
				Payload: payload, IdempotencyKey: "label-provisioning:delivery:9123",
			},
			ID:         "40000000-0000-4000-8000-000000000001",
			LeaseToken: "50000000-0000-4000-8000-000000000001",
		},
		Attempt: 1,
	}
}

func provisioningWorker(durable *provisioningStore, credentials *provisioningCredentials, api *provisioningAPI) *githubapi.LabelProvisioningWorker {
	worker, err := githubapi.NewLabelProvisioningWorker(durable, credentials, api, githubapi.LabelProvisioningWorkerConfig{
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
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{
		listed:     []githubapi.InstallationRepository{{ID: 9123, Owner: "jozala", Name: "omnigrex"}},
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		labels:     []githubapi.Label{{Name: "omnigrex:run", Color: "old", Description: "old"}},
	}
	worker := provisioningWorker(durable, credentials, api)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want success", processed, err)
	}
	if len(durable.completed) != 1 || len(durable.failed) != 0 {
		t.Fatalf("completed/failed = (%d, %d), want (1, 0)", len(durable.completed), len(durable.failed))
	}
	if credentials.installationID != 99 {
		t.Errorf("installation credential ID = %d, want 99", credentials.installationID)
	}
	if len(api.created) != 4 {
		t.Errorf("created labels = %#v, want four missing labels", api.created)
	}
	for _, name := range api.created {
		if name == "omnigrex:run" {
			t.Errorf("existing omnigrex:run was recreated")
		}
	}
	if durable.claimQueue != store.LabelProvisioningQueue || durable.claimKind != store.ProvisionManagedLabelsJobKind || durable.claimOwner != "provisioner" {
		t.Errorf("claim = (%q, %q, %q), want label provisioning queue/kind owned by provisioner",
			durable.claimQueue, durable.claimKind, durable.claimOwner)
	}
}

func TestLabelProvisioningWorkerResolvesRenamedRepositoryByID(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{
		listed:     []githubapi.InstallationRepository{{ID: 9123, Owner: "jozala", Name: "renamed"}},
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "renamed"},
	}
	worker := provisioningWorker(durable, credentials, api)

	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessNext() = (%t, %v), want renamed repository provisioned", processed, err)
	}
	if len(durable.completed) != 1 {
		t.Fatalf("completed = %d, want 1", len(durable.completed))
	}
	if len(api.created) != 5 {
		t.Errorf("created labels = %#v, want all five managed labels under the new name", api.created)
	}
}

func TestLabelProvisioningWorkerFailsTerminallyOnLostAccess(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{err: &githubapi.NotInstalledError{InstallationID: 99}}
	api := &provisioningAPI{}
	worker := provisioningWorker(durable, credentials, api)

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

func TestLabelProvisioningWorkerFailsTerminallyWhenRepositoryRemoved(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{
		listed: []githubapi.InstallationRepository{{ID: 9999, Owner: "jozala", Name: "other"}},
	}
	worker := provisioningWorker(durable, credentials, api)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want terminal removal failure", processed, err)
	}
	if len(durable.failed) != 1 || durable.failed[0].retryable {
		t.Errorf("failures = %#v, want one terminal removal failure", durable.failed)
	}
}

func TestLabelProvisioningWorkerFailsTerminallyOnCorruptPayload(t *testing.T) {
	lease := provisioningLease()
	lease.Payload = json.RawMessage(`{"repository_id":0}`)
	durable := &provisioningStore{lease: lease}
	worker := provisioningWorker(durable, &provisioningCredentials{}, &provisioningAPI{})

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want terminal payload failure", processed, err)
	}
	if len(durable.failed) != 1 || durable.failed[0].retryable {
		t.Errorf("failures = %#v, want one terminal payload failure", durable.failed)
	}
}

func TestLabelProvisioningWorkerRetriesTransientLabelFailure(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{
		listed:     []githubapi.InstallationRepository{{ID: 9123, Owner: "jozala", Name: "omnigrex"}},
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		createErr:  &githubapi.TransientError{Cause: errors.New("unavailable")},
	}
	worker := provisioningWorker(durable, credentials, api)

	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessNext() = (%t, %v), want retryable failure", processed, err)
	}
	if len(durable.failed) != 1 || !durable.failed[0].retryable {
		t.Errorf("failures = %#v, want one retryable failure", durable.failed)
	}
}
