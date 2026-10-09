// Command fixture serves the real fenced MCP gateway with disposable authority
// and an in-memory backend. It has no GitHub credentials or external mutations.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/store"
)

type fixture struct {
	mu          sync.Mutex
	invocations []string
	revoked     bool
	mutation    store.MutationReservation
}

func (f *fixture) ValidateTurnFence(context.Context, store.AgentTurnLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked {
		return errors.New("fixture fence revoked")
	}
	return nil
}
func (f *fixture) ReserveMutation(_ context.Context, lease store.AgentTurnLease, spec store.MutationSpec) (store.MutationReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutation = store.MutationReservation{ID: "fixture-mutation", AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch, State: store.MutationReserved, Request: spec.Request, ToolName: spec.ToolName, OperationID: spec.OperationID}
	return f.mutation, nil
}
func (*fixture) AcknowledgeMutationReplay(context.Context, store.AgentTurnLease, string, store.MutationSpec) error {
	return errors.New("fixture mutations disabled")
}
func (f *fixture) StartMutation(context.Context, store.AgentTurnLease, string) (store.MutationReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutation.State = store.MutationInFlight
	return f.mutation, nil
}
func (*fixture) RecordProposedPublicationTip(context.Context, store.AgentTurnLease, string, string) error {
	return nil
}
func (*fixture) CompleteMutation(context.Context, store.AgentTurnLease, string, json.RawMessage) error {
	return nil
}
func (*fixture) FailMutation(context.Context, store.AgentTurnLease, string, error) error {
	return errors.New("fixture mutations disabled")
}
func (*fixture) MarkMutationUnknown(context.Context, store.AgentTurnLease, string, error) error {
	return errors.New("fixture mutations disabled")
}
func (f *fixture) Execute(_ context.Context, invocation mcp.Invocation) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invocations = append(f.invocations, string(invocation.Scope.Role)+":"+invocation.Name)
	return json.RawMessage(`{"fixture":"authorized"}`), nil
}

func main() {
	f := &fixture{invocations: []string{}}
	random := append(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)...)
	gateway, err := mcp.New(mcp.Config{EndpointURL: "http://127.0.0.1:8844/mcp", Store: f, Backend: f, Random: bytes.NewReader(random)})
	if err != nil {
		panic(err)
	}
	for _, id := range []role.ID{role.Developer, role.Reviewer} {
		expires := time.Now().Add(10 * time.Minute)
		scope := mcp.TokenScope{
			RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v1", WorkflowID: "fixture-workflow", Role: id,
			Lease: store.AgentTurnLease{
				AgentTurn: store.AgentTurn{ID: "fixture-turn-" + string(id), AgentAssignmentID: "fixture-participant-" + string(id), AgentSessionID: "fixture-session-" + string(id), ExecutionEpoch: 1, CreatedAt: time.Now(), AgentProfileConfig: json.RawMessage(fmt.Sprintf(`{"name":"fixture","role":%q}`, id))},
				JobLease:  store.JobLease{WorkflowID: "fixture-workflow", AgentTurnID: "fixture-turn-" + string(id), AgentSessionID: "fixture-session-" + string(id), AgentAssignmentID: "fixture-participant-" + string(id), ExecutionEpoch: 1},
				OwnerID:   "fixture", OwnerToken: "fixture-owner", LeaseExpiresAt: expires,
			},
			Repository: mcp.RepositoryScope{ID: 1, Owner: "fixture", Name: "repository"}, Issue: mcp.IssueScope{ID: 1, Number: 1},
			Branch: "omnigrex/issue-1", DefaultBranch: "main", HeadSHA: "1111111111111111111111111111111111111111", ExpiresAt: expires,
		}
		if id == role.Reviewer {
			scope.PullRequest = &mcp.PullRequestScope{ID: 2, Number: 2}
			scope.Lease.ChangeProposalID = "fixture-proposal"
			scope.Lease.ExpectedHeadSHA = scope.HeadSHA
		}
		if _, err := gateway.Register(scope); err != nil {
			panic(err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", gateway)
	mux.HandleFunc("/audit", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.invocations)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.revoked = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	fmt.Fprintln(os.Stderr, "Pi fixture gateway listening")
	server := &http.Server{Addr: "127.0.0.1:8844", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
