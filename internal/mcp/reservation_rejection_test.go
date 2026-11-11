package mcp_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
)

func TestDefiniteReservationRejectionAllowsYieldWithoutErasingAmbiguity(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "definite-only", true: "earlier-ambiguous"}[ambiguous], func(t *testing.T) {
			now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			durable := &rejectionStore{fakeStore: &fakeStore{}, ambiguous: ambiguous}
			backend := &recordingBackend{result: []byte(`{"outcome":"CLARIFICATION_WAIT","round_id":""}`)}
			gateway, err := mcp.New(mcp.Config{EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			registration, err := gateway.Register(validScope(now))
			if err != nil {
				t.Fatal(err)
			}
			initialize(t, gateway, registration)
			for range 2 {
				response := httptest.NewRecorder()
				gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"request_review","arguments":{"operation_id":"blocked","summary":"Ready"}}}`))
				if !strings.Contains(response.Body.String(), `"isError":true`) {
					t.Fatal(response.Body.String())
				}
			}
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"request_clarification","arguments":{"operation_id":"yield","questions":[]}}}`))
			if strings.Contains(response.Body.String(), `"isError":true`) || !strings.Contains(response.Body.String(), "CLARIFICATION_WAIT") {
				t.Fatal(response.Body.String())
			}
			if len(durable.mutations) != 1 || backend.count() != 1 {
				t.Fatalf("ledger=%+v calls=%d", durable.mutations, backend.count())
			}
			err = gateway.CloseAndDrain(t.Context(), registration)
			if ambiguous && !errors.Is(err, mcp.ErrMutationDrainUnresolved) || !ambiguous && err != nil {
				t.Fatalf("drain=%v", err)
			}
		})
	}
}

type rejectionStore struct {
	*fakeStore
	ambiguous bool
	calls     int
}

func (s *rejectionStore) ReserveMutation(ctx context.Context, lease store.AgentTurnLease, spec store.MutationSpec) (store.MutationReservation, error) {
	if spec.ToolName == mcp.ToolRequestReview {
		s.calls++
		if s.ambiguous && s.calls == 1 {
			return store.MutationReservation{}, errors.New("lost commit acknowledgment")
		}
		return store.MutationReservation{}, &store.MutationReservationRejection{Cause: store.ErrPendingHumanInput}
	}
	return s.fakeStore.ReserveMutation(ctx, lease, spec)
}
