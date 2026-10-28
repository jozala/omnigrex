package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/gitdelivery"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type refreshStore struct {
	fakeStore
	fenceErr error
	fences   int
}

func (s *refreshStore) WithSnapshotInstallationFence(ctx context.Context, lease store.AgentTurnLease, key execution.Key, apply func(context.Context) error) error {
	s.fences++
	if s.fenceErr != nil {
		return s.fenceErr
	}
	return apply(ctx)
}

type refreshAPI struct {
	backendGitHub
	observations []githubapi.DefaultBranch
	calls        int
	credential   string
	failure      error
	repositoryID int64
}

func (a *refreshAPI) GetRepository(_ context.Context, _ string, owner, repo string) (githubapi.InstallationRepository, error) {
	id := a.repositoryID
	if id == 0 {
		id = 9123
	}
	return githubapi.InstallationRepository{ID: id, Owner: owner, Name: repo}, nil
}

func (a *refreshAPI) ResolveDefaultBranch(_ context.Context, credential, owner, repo string) (githubapi.DefaultBranch, error) {
	a.credential = credential
	i := min(a.calls, len(a.observations)-1)
	a.calls++
	if a.failure != nil {
		return githubapi.DefaultBranch{}, a.failure
	}
	return a.observations[i], nil
}

type refreshAcquirer struct {
	calls                        int
	remote, credential, sha, ref string
	failure                      error
}

func (a *refreshAcquirer) AcquireSnapshot(_ context.Context, remote, credential, sha, ref string) (gitdelivery.Snapshot, error) {
	a.calls++
	a.remote, a.credential, a.sha, a.ref = remote, credential, sha, ref
	return gitdelivery.Snapshot{SHA: sha, Ref: ref, PackName: strings.Repeat("c", 40), Pack: []byte("pack"), Index: []byte("idx")}, a.failure
}

type refreshInstaller struct {
	calls    int
	ref      execution.WorkspaceRef
	snapshot gitdelivery.Snapshot
	failure  error
	entered  chan struct{}
	release  <-chan struct{}
}

func (i *refreshInstaller) InstallSnapshot(ctx context.Context, ref execution.WorkspaceRef, snapshot gitdelivery.Snapshot, fence execution.WorkspaceFence) error {
	return fence(ctx, func(context.Context) error { return i.install(ref, snapshot) })
}
func (i *refreshInstaller) install(ref execution.WorkspaceRef, snapshot gitdelivery.Snapshot) error {
	i.calls++
	i.ref = ref
	i.snapshot = snapshot
	if i.entered != nil {
		i.entered <- struct{}{}
	}
	if i.release != nil {
		<-i.release
	}
	return i.failure
}

func TestRefreshGatewaySnapshotFreshnessReplayAndRoleBoundaries(t *testing.T) {
	b, c := strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		for _, status := range []string{"current", "stale", "default_branch_changed", "unavailable"} {
			t.Run(string(role)+"/"+status, func(t *testing.T) {
				api := &refreshAPI{observations: []githubapi.DefaultBranch{{Name: "trunk", CommitSHA: b}, {Name: "trunk", CommitSHA: b}}}
				switch status {
				case "stale":
					api.observations[1].CommitSHA = c
				case "default_branch_changed":
					api.observations[0].Name = "renamed"
				case "unavailable":
					api.failure = errors.New("network failure with secret")
				}
				acquirer := &refreshAcquirer{}
				installer := &refreshInstaller{}
				durable := &refreshStore{}
				credentials := &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"}
				backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}, Snapshots: acquirer, Installer: installer})
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				gateway, err := mcp.New(mcp.Config{EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend, Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				scope := validScope(now)
				scope.Role = role
				scope.DefaultBranch = "trunk"
				scope.HeadSHA = productionHeadSHA
				scope.Lease.AgentProfileConfig = json.RawMessage(fmt.Sprintf(`{"name":"fixture","role":%q}`, role))
				if role == workflow.RoleReviewer {
					scope.PullRequest = &mcp.PullRequestScope{ID: 789, Number: 23}
					scope.Lease.ChangeProposalID = "proposal"
					scope.Lease.ExpectedHeadSHA = scope.HeadSHA
				}
				registration, err := gateway.Register(scope)
				if err != nil {
					t.Fatal(err)
				}
				initialize(t, gateway, registration)
				call := func(op string, extra string) string {
					t.Helper()
					body := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"refresh_default_branch","arguments":{"operation_id":%q%s}}}`, op, extra)
					response := httptest.NewRecorder()
					gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
					return response.Body.String()
				}
				first := call("observe-1", "")
				if strings.Contains(first, `"isError":true`) || !strings.Contains(first, `\"status\":\"`+status+`\"`) {
					t.Fatal(first)
				}
				if got := call("observe-1", ""); got != first {
					t.Fatalf("replay changed observation: %s -> %s", first, got)
				}
				wantCalls := 2
				if status == "default_branch_changed" || status == "unavailable" {
					wantCalls = 1
				}
				if api.calls != wantCalls {
					t.Fatalf("replay re-observed: %d", api.calls)
				}
				if credentials.reviewerCalls != 0 || api.credential != "developer-secret" {
					t.Fatal("wrong credential authority")
				}
				if status == "current" || status == "stale" {
					if installer.calls != 1 || durable.fences != 1 || installer.ref.Key.AgentTurnID != scope.Lease.ID || acquirer.sha != b || installer.snapshot.Ref != acquirer.ref {
						t.Fatal("installation not scoped/fenced")
					}
					if !strings.Contains(first, b) || !strings.Contains(first, acquirer.ref) {
						t.Fatal("missing usable provenance")
					}
				}
				if got := call("observe-2", `,"ref":"refs/heads/other"`); !strings.Contains(got, `"error"`) {
					t.Fatal("accepted arbitrary ref", got)
				}
				before := api.calls
				call("observe-2", "")
				if api.calls <= before {
					t.Fatal("new identity did not observe afresh")
				}
			})
		}
	}
}

func TestRefreshGatewayLostFenceNeverInstalls(t *testing.T) {
	api := &refreshAPI{observations: []githubapi.DefaultBranch{{Name: "main", CommitSHA: productionHeadSHA}}}
	installer := &refreshInstaller{}
	durable := &refreshStore{fenceErr: store.ErrAgentTurnFenceLost}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "secret"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}, Snapshots: &refreshAcquirer{}, Installer: installer})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	gateway, err := mcp.New(mcp.Config{EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(now)
	scope.HeadSHA = productionHeadSHA
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	initialize(t, gateway, registration)
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"refresh_default_branch","arguments":{"operation_id":"fenced"}}}`
	for range 2 {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		if !strings.Contains(response.Body.String(), `"isError":true`) {
			t.Fatal(response.Body.String())
		}
	}
	if installer.calls != 0 || api.calls != 1 {
		t.Fatal("lost fence installed or replay reacquired")
	}
}

func TestRefreshGatewayFailureAndRepositoryIdentity(t *testing.T) {
	for _, failure := range []string{"wrong_repository", "acquisition", "installation", "default_renamed_after_delivery"} {
		t.Run(failure, func(t *testing.T) {
			api := &refreshAPI{observations: []githubapi.DefaultBranch{{Name: "main", CommitSHA: productionHeadSHA}, {Name: "main", CommitSHA: productionHeadSHA}}}
			acquirer := &refreshAcquirer{}
			installer := &refreshInstaller{}
			durable := &refreshStore{}
			switch failure {
			case "wrong_repository":
				api.repositoryID = 777
			case "acquisition":
				acquirer.failure = errors.New("upstream secret diagnostic")
			case "installation":
				installer.failure = context.DeadlineExceeded
			case "default_renamed_after_delivery":
				api.observations[1].Name = "renamed"
			}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "secret"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}, Snapshots: acquirer, Installer: installer})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			gateway, err := mcp.New(mcp.Config{EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			scope := validScope(now)
			scope.HeadSHA = productionHeadSHA
			registration, err := gateway.Register(scope)
			if err != nil {
				t.Fatal(err)
			}
			initialize(t, gateway, registration)
			body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"refresh_default_branch","arguments":{"operation_id":"same-snapshot"}}}`
			first := ""
			for range 2 {
				response := httptest.NewRecorder()
				gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
				got := response.Body.String()
				if strings.Contains(got, "secret") {
					t.Fatal("diagnostics leaked")
				}
				if failure == "installation" {
					if !strings.Contains(got, `"isError":true`) {
						t.Fatal(got)
					}
				} else {
					status := "unavailable"
					if failure == "default_renamed_after_delivery" {
						status = "default_branch_changed"
					}
					if !strings.Contains(got, `\"status\":\"`+status+`\"`) {
						t.Fatal(got)
					}
					if first != "" && got != first {
						t.Fatal("lost-response replay changed result")
					}
					first = got
				}
			}
			if failure == "wrong_repository" && (api.calls != 0 || acquirer.calls != 0 || installer.calls != 0) {
				t.Fatal("repository mismatch broadened access")
			}
			if failure == "installation" && installer.calls != 1 {
				t.Fatal("interrupted installation was repeated")
			}
		})
	}
}

func TestRefreshLostResponseAndConcurrentDuplicateUseOneSnapshot(t *testing.T) {
	api := &refreshAPI{observations: []githubapi.DefaultBranch{{Name: "main", CommitSHA: productionHeadSHA}}}
	acquirer := &refreshAcquirer{}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	installer := &refreshInstaller{entered: entered, release: release}
	durable := &refreshStore{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "secret"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}, Snapshots: acquirer, Installer: installer})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	gateway, err := mcp.New(mcp.Config{EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(now)
	scope.HeadSHA = productionHeadSHA
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	initialize(t, gateway, registration)
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"refresh_default_branch","arguments":{"operation_id":"lost-response"}}}`
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		request := rpcRequest(t, registration, mcp.ProtocolVersion, body).WithContext(ctx)
		gateway.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("installation not admitted")
	}
	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("lost-response caller did not detach")
	}
	replay := make(chan string, 1)
	go func() {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		replay <- response.Body.String()
	}()
	select {
	case <-entered:
		t.Fatal("concurrent replay installed again")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-replay:
		if !strings.Contains(result, `\"status\":\"current\"`) {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("replay failed to resume")
	}
	if installer.calls != 1 || acquirer.calls != 1 || api.calls != 2 {
		t.Fatalf("duplicate reacquired: %d %d %d", installer.calls, acquirer.calls, api.calls)
	}
}
