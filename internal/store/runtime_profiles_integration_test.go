//go:build integration

package store_test

import (
	"context"

	"errors"

	"strings"
	"testing"
	"time"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
)

func TestProtectedRuntimeBindingsRemainUntilCollectionFinalization(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binding := protectedTestBinding("a", "1")
	fixture := seedAgentSessionWithRuntimeBindings(t, pool, 61, "DEVELOPER", binding, binding)

	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{binding})
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, pool, ctx, fixture, 61,
		"61000000-0000-4000-8000-000000000611", "protected-closure", "protected-retention",
		observedAt, observedAt.Add(time.Hour))
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "protected-collector", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}
	authorization, err := database.AuthorizeAssignmentCollection(ctx, *lease)
	if err != nil {
		t.Fatalf("AuthorizeAssignmentCollection() error = %v", err)
	}
	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{binding})
	if _, err := database.FinalizeAssignmentCollection(ctx, *lease, authorization.Targets[:1]); !errors.Is(err, store.ErrAssignmentCollectionIncomplete) {
		t.Fatalf("partial FinalizeAssignmentCollection() error = %v", err)
	}
	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{binding})

	if _, err := database.FinalizeAssignmentCollection(ctx, *lease, authorization.Targets); err != nil {
		t.Fatalf("FinalizeAssignmentCollection() error = %v", err)
	}
	assertProtectedBindings(t, database, ctx, nil)
}

func TestProtectedRuntimeBindingsIncludeDistinctActiveAndRetainedDigests(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	activeBinding := protectedTestBinding("b", "2")
	retainedBinding := protectedTestBinding("c", "3")
	seedAgentSessionWithRuntimeBindings(t, pool, 62, "DEVELOPER", activeBinding, activeBinding)
	retained := seedAgentSessionWithRuntimeBindings(t, pool, 63, "DEVELOPER", retainedBinding, retainedBinding)
	if _, err := pool.Exec(ctx, `
UPDATE agent_participants SET status = 'COMPLETED', completed_at = clock_timestamp(), retention_until = clock_timestamp() + interval '1 day'
WHERE id = $1`, retained.assignmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'RETAINED', retained_at = clock_timestamp() WHERE id = $1`, retained.sessionID); err != nil {
		t.Fatal(err)
	}

	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{activeBinding, retainedBinding})
}

func TestProtectedRuntimeBindingsRejectInconsistentAssignmentAndSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assignment := protectedTestBinding("d", "4")
	session := protectedTestBinding("e", "5")
	seedAgentSessionWithRuntimeBindings(t, pool, 64, "DEVELOPER", assignment, session)

	if _, err := database.ListProtectedRuntimeBindings(ctx); !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) {
		t.Fatalf("ListProtectedRuntimeBindings() error = %v, want configuration conflict", err)
	}
}

func TestProtectedRuntimeBindingsRejectMutableTagsAndLocalImageIDs(t *testing.T) {
	for index, image := range []string{
		"registry.example/omnigrex/pi:latest",
		"sha256:" + strings.Repeat("f", 64),
	} {
		t.Run(image[:6], func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			binding := protectedTestBinding("f", "6")
			binding.Image = image
			seedAgentSessionWithRuntimeBindings(t, pool, 65+index, "DEVELOPER", binding, binding)
			if _, err := database.ListProtectedRuntimeBindings(ctx); !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) {
				t.Fatalf("ListProtectedRuntimeBindings() error = %v, want configuration conflict", err)
			}
		})
	}
}

func protectedTestBinding(hashDigit, imageDigit string) runtimeprofile.Binding {
	return runtimeprofile.Binding{
		Name: "pi-rpc", Version: "v2", ContentSHA256: strings.Repeat(hashDigit, 64),
		Image: "registry.example/omnigrex/pi@sha256:" + strings.Repeat(imageDigit, 64),
	}
}

func assertProtectedBindings(t *testing.T, database *store.Store, ctx context.Context, want []runtimeprofile.Binding) {
	t.Helper()
	got, err := database.ListProtectedRuntimeBindings(ctx)
	if err != nil {
		t.Fatalf("ListProtectedRuntimeBindings() error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListProtectedRuntimeBindings() = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ListProtectedRuntimeBindings()[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}
