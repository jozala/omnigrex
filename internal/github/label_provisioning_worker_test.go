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
	lease      store.JobLease
	cause      error
	retryable  bool
	retryDelay time.Duration
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

func (durable *provisioningStore) FailJob(_ context.Context, lease store.JobLease, cause error, retryable bool, retryDelay time.Duration) error {
	durable.failed = append(durable.failed, provisioningFailure{lease: lease, cause: cause, retryable: retryable, retryDelay: retryDelay})
	return nil
}

type provisioningCredentials struct {
	token          string
	installationID int64
	verifiedID     int64
	verifiedOwner  string
	verifiedName   string
	verifyErr      error
	err            error
}

func (provider *provisioningCredentials) InstallationCredential(_ context.Context, installationID int64) (string, error) {
	provider.installationID = installationID
	return provider.token, provider.err
}

func (provider *provisioningCredentials) VerifyRepositoryInstallation(_ context.Context, installationID int64, owner, repository string) error {
	provider.verifiedID, provider.verifiedOwner, provider.verifiedName = installationID, owner, repository
	return provider.verifyErr
}

type provisioningAPI struct {
	githubapi.LabelAPI
	byID          githubapi.InstallationRepository
	byIDAgain     *githubapi.InstallationRepository
	byIDErr       error
	byIDCalls     int
	requestedID   int64
	repository    githubapi.InstallationRepository
	repositoryErr error
	getCalls      int
	labels        []githubapi.Label
	created       []string
	createErr     error
}

func (api *provisioningAPI) GetRepositoryByID(_ context.Context, _ string, repositoryID int64) (githubapi.InstallationRepository, error) {
	api.byIDCalls++
	api.requestedID = repositoryID
	if api.byIDCalls > 1 && api.byIDAgain != nil {
		return *api.byIDAgain, nil
	}
	return api.byID, api.byIDErr
}

func (api *provisioningAPI) GetRepository(context.Context, string, string, string) (githubapi.InstallationRepository, error) {
	api.getCalls++
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
		byID:       githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
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
	if credentials.verifiedID != 99 || credentials.verifiedOwner != "jozala" || credentials.verifiedName != "omnigrex" || api.byIDCalls != 1 || api.requestedID != 9123 {
		t.Errorf("repository verification = (%d, %q, %q), by-ID lookup = (%d, %d)", credentials.verifiedID, credentials.verifiedOwner, credentials.verifiedName, api.byIDCalls, api.requestedID)
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
		byID:       githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "renamed"},
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
	if credentials.verifiedName != "renamed" || api.byIDCalls != 1 {
		t.Errorf("verified name = %q, lookups = %d, want one lookup of current name", credentials.verifiedName, api.byIDCalls)
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
		byIDErr: &githubapi.APIError{StatusCode: 404, Method: "GET", Path: "/repositories/9123"},
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

func TestLabelProvisioningWorkerRejectsPublicRepositoryRemovedFromInstallation(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token", verifyErr: &githubapi.NotInstalledError{Owner: "jozala", Repository: "omnigrex"}}
	api := &provisioningAPI{byID: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"}}
	worker := provisioningWorker(durable, credentials, api)
	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || len(durable.failed) != 1 || durable.failed[0].retryable {
		t.Fatalf("ProcessNext() = (%t, %v), failures = %#v, want terminal lost-access failure", processed, err, durable.failed)
	}
	if api.getCalls != 0 || len(api.created) != 0 {
		t.Errorf("named reads = %d, created = %v, want no further repository access", api.getCalls, api.created)
	}
}

func TestLabelProvisioningWorkerRetriesRenameDuringInstallationVerification(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token", verifyErr: &githubapi.NotInstalledError{Owner: "jozala", Repository: "old-name"}}
	newName := githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "new-name"}
	api := &provisioningAPI{
		byID:      githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "old-name"},
		byIDAgain: &newName,
	}
	processed, err := provisioningWorker(durable, credentials, api).ProcessNext(context.Background())
	if !processed || err == nil || len(durable.failed) != 1 || !durable.failed[0].retryable || api.byIDCalls != 2 {
		t.Fatalf("ProcessNext() = (%t, %v), failures = %#v, lookups = %d; want retry of changed name", processed, err, durable.failed, api.byIDCalls)
	}
	if api.getCalls != 0 || len(api.created) != 0 {
		t.Error("a stale repository name was used after App verification failed")
	}
}

func TestLabelProvisioningWorkerRejectsMismatchedRepositoryByID(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{byID: githubapi.InstallationRepository{ID: 9999, Owner: "jozala", Name: "other"}}
	worker := provisioningWorker(durable, credentials, api)
	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || len(durable.failed) != 1 || durable.failed[0].retryable {
		t.Fatalf("ProcessNext() = (%t, %v), failures = %#v, want terminal identity failure", processed, err, durable.failed)
	}
	if credentials.verifiedID != 0 || api.getCalls != 0 || len(api.created) != 0 {
		t.Error("mismatched repository identity was used for further GitHub operations")
	}
}

func TestLabelProvisioningWorkerRejectsNameChangedAfterResolution(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{
		byID:       githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "before"},
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "after"},
	}
	worker := provisioningWorker(durable, credentials, api)
	processed, err := worker.ProcessNext(context.Background())
	if !processed || err == nil || len(durable.failed) != 1 || !durable.failed[0].retryable || len(api.created) != 0 {
		t.Fatalf("ProcessNext() = (%t, %v), failures = %#v, labels = %v; want retry without mutations", processed, err, durable.failed, api.created)
	}
}

func TestLabelProvisioningWorkerRetriesNameDisappearingAfterResolution(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	credentials := &provisioningCredentials{token: "installation-token"}
	api := &provisioningAPI{
		byID:          githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "old-name"},
		repositoryErr: &githubapi.APIError{StatusCode: 404, Method: "GET", Path: "/repos/jozala/old-name"},
	}
	processed, err := provisioningWorker(durable, credentials, api).ProcessNext(context.Background())
	if !processed || err == nil || len(durable.failed) != 1 || !durable.failed[0].retryable || len(api.created) != 0 {
		t.Fatalf("ProcessNext() = (%t, %v), failures = %#v, labels = %v; want retry after name change", processed, err, durable.failed, api.created)
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
		byID:       githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
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

func TestLabelProvisioningWorkerWaitsForLaterRateLimitReset(t *testing.T) {
	durable := &provisioningStore{lease: provisioningLease()}
	reset := time.Now().Add(time.Minute)
	api := &provisioningAPI{
		byID:       githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		repository: githubapi.InstallationRepository{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		createErr: &githubapi.RateLimitError{
			APIError:   &githubapi.APIError{StatusCode: 429, Method: "POST", Path: "/repos/jozala/omnigrex/labels"},
			RetryAfter: time.Second, ResetAt: reset,
		},
	}
	processed, err := provisioningWorker(durable, &provisioningCredentials{token: "installation-token"}, api).ProcessNext(context.Background())
	if !processed || err == nil || len(durable.failed) != 1 || !durable.failed[0].retryable {
		t.Fatalf("ProcessNext() = (%t, %v), failures = %#v, want retryable rate limit", processed, err, durable.failed)
	}
	if delay := durable.failed[0].retryDelay; delay < 50*time.Second || delay > time.Minute {
		t.Errorf("retry delay = %s, want reset in approximately one minute rather than one-second Retry-After", delay)
	}
}
