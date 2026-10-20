//go:build integration

package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

func TestWorkspaceCaptureLedgerSurvivesStoreReopenWithOneConnection(t *testing.T) {
	postgres := startPostgres(t)
	password := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(password, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func() *store.Store {
		t.Helper()
		database, err := openTimedStore(t, ctx, postgres.databaseURL(false)+"&pool_max_conns=1", password, builtinStoreConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		return database
	}
	database := open()
	t.Cleanup(database.Close)
	pool := openPool(t, postgres.databaseURL(true))
	ref, lease := workspaceLedgerFixture(t, database, pool, 1701)
	if _, found, err := database.LoadCapture(ctx, ref.Request); err != nil || found {
		t.Fatalf("unsealed capture = %t, %v", found, err)
	}
	// Source admission finishes before recording facts. Calling the ledger from
	// inside this fence would deadlock the one-connection pool.
	source := &ledgerIntegrationCaptureSource{archive: workspaceLedgerArchive(t, "original bytes")}
	source.fence = func(ctx context.Context, operation func(context.Context) error) error {
		return database.WithAgentTurnFence(ctx, lease, operation)
	}
	before := workspaceLedgerCoordinationState(t, pool)
	artifact := filepath.Join(t.TempDir(), "workspace.tar")
	file, err := os.Create(artifact)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	err = source.ReadWorkspace(ctx, ref.Request, func(reader io.Reader) error {
		_, err := io.Copy(io.MultiWriter(file, hash), reader)
		return err
	})
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil || source.calls != 1 {
		t.Fatalf("admitted capture with one connection = %v; source calls %d", err, source.calls)
	}
	ref.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatalf("repeat seal: %v", err)
	}
	if after := workspaceLedgerCoordinationState(t, pool); after != before {
		t.Fatal("recording capture facts mutated coordination state")
	}
	var requestJSON []byte
	var count int
	if err := pool.QueryRow(ctx, `SELECT request, (SELECT count(*) FROM workspace_captures) FROM workspace_captures`).Scan(&requestJSON, &count); err != nil {
		t.Fatal(err)
	}
	var persisted execution.CaptureRequest
	if err := json.Unmarshal(requestJSON, &persisted); err != nil || persisted != ref.Request || count != 1 {
		t.Fatalf("persisted request = %#v, count %d, error %v", persisted, count, err)
	}
	database.Close()
	database = open()
	t.Cleanup(database.Close)
	loaded, found, err := database.LoadCapture(ctx, ref.Request)
	if err != nil || !found || loaded != ref {
		t.Fatalf("reopened capture = %#v, %t, %v; want %#v", loaded, found, err, ref)
	}
	// A lost seal response is safely retried with exactly the same reference.
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatalf("seal after reopen: %v", err)
	}
}

func TestWorkspaceCaptureLedgerKeepsSealedIdentityAfterEntireArtifactLoss(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ref, _ := workspaceLedgerFixture(t, database, pool, 1705)
	ctx := context.Background()
	artifactDirectory := t.TempDir()
	archive := workspaceLedgerArchive(t, "original bytes")
	digest := sha256.Sum256(archive)
	ref.SHA256 = hex.EncodeToString(digest[:])
	if err := os.WriteFile(filepath.Join(artifactDirectory, "workspace.tar"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(artifactDirectory); err != nil {
		t.Fatal(err)
	}
	if loaded, found, err := database.LoadCapture(ctx, ref.Request); err != nil || !found || loaded != ref {
		t.Fatalf("missing artifact lost sealed identity = %#v, %t, %v", loaded, found, err)
	}
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatalf("missing artifact changed idempotent ledger facts: %v", err)
	}
	replacement := ref
	digest = sha256.Sum256(workspaceLedgerArchive(t, "newer bytes"))
	replacement.SHA256 = hex.EncodeToString(digest[:])
	if err := database.SealCapture(ctx, replacement); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("replacement after entire artifact loss = %v, want conflict", err)
	}
}

func TestWorkspaceCaptureLedgerRejectsReinterpretationAndInvalidDurableKeys(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx := context.Background()
	ref, _ := workspaceLedgerFixture(t, database, pool, 1702)
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*execution.CaptureRef)
	}{
		{"digest", func(ref *execution.CaptureRef) { ref.SHA256 = strings.Repeat("b", 64) }},
		{"seed", func(ref *execution.CaptureRef) { ref.Request.Workspace.Seed = "another seed" }},
		{"Participant", func(ref *execution.CaptureRef) {
			ref.Request.Workspace.Key.AssignmentID = "10000000-0000-4000-8000-000000000099"
		}},
		{"Session", func(ref *execution.CaptureRef) {
			ref.Request.Workspace.Key.AgentSessionID = "20000000-0000-4000-8000-000000000099"
		}},
		{"Runtime Profile name", func(ref *execution.CaptureRef) { ref.Request.Workspace.Key.RuntimeProfileName = "other" }},
		{"Runtime Profile version", func(ref *execution.CaptureRef) { ref.Request.Workspace.Key.RuntimeProfileVersion = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrong := ref
			test.edit(&wrong)
			if err := database.SealCapture(ctx, wrong); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("changed seal = %v, want conflict", err)
			}
			if wrong.Request != ref.Request {
				if _, _, err := database.LoadCapture(ctx, wrong.Request); !errors.Is(err, execution.ErrConflict) {
					t.Fatalf("changed load = %v, want conflict", err)
				}
				// An unused operation cannot establish a bogus durable binding either.
				wrong.Request.OperationID = "unused operation"
				if test.name != "seed" {
					if err := database.SealCapture(ctx, wrong); !errors.Is(err, execution.ErrConflict) {
						t.Fatalf("invalid new binding = %v, want conflict", err)
					}
				}
			}
		})
	}
	for _, edit := range []func(*execution.CaptureRef){
		func(ref *execution.CaptureRef) {
			ref.Request.Workspace.Key.AgentTurnID = "30000000-0000-4000-8000-000000000099"
		},
		func(ref *execution.CaptureRef) { ref.Request.Workspace.Key.ExecutionEpoch++ },
	} {
		wrong := ref
		edit(&wrong)
		if err := database.SealCapture(ctx, wrong); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("unknown Turn/epoch = %v, want conflict", err)
		}
	}
	// Session and Participant profile bindings must agree, not just match input.
	// Legal same-version column values must not conceal a different full binding.
	for index, mutation := range []struct{ name, query string }{
		{"contract", `UPDATE agent_sessions SET runtime_profile_content_sha256 = repeat('b', 64) WHERE id = $1`},
		{"image", `UPDATE agent_sessions SET runtime_image_digest = 'other-image' WHERE id = $1`},
		{"state", `UPDATE agent_sessions SET runtime_state_path = 'other-state' WHERE id = $1`},
	} {
		inconsistent := seedCreatingAgentSession(t, pool, 1799+index)
		turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, inconsistent.turnSpec())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, mutation.query, inconsistent.sessionID); err != nil {
			t.Fatal(err)
		}
		newRef := ref
		newRef.Request.OperationID = "new capture with inconsistent Session " + mutation.name
		newRef.Request.Workspace.Key.AssignmentID = inconsistent.assignmentID
		newRef.Request.Workspace.Key.AgentSessionID = inconsistent.sessionID
		newRef.Request.Workspace.Key.AgentTurnID = turn.ID
		newRef.Request.Workspace.Key.ExecutionEpoch = turn.ExecutionEpoch
		if err := database.SealCapture(ctx, newRef); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("inconsistent Session %s = %v", mutation.name, err)
		}
	}
	// Existing evidence remains readable and retryable independently of admission.
	if got, found, err := database.LoadCapture(ctx, ref.Request); err != nil || !found || got != ref {
		t.Fatalf("original evidence = %#v, %t, %v", got, found, err)
	}
	if err := database.SealCapture(ctx, ref); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE workspace_captures SET sha256 = repeat('b', 64)`,
		`UPDATE workspace_captures SET request = jsonb_set(request, '{Workspace,Seed}', '"replacement"')`,
		`DELETE FROM workspace_captures`,
	} {
		if _, err := pool.Exec(ctx, query); err == nil {
			t.Fatalf("sealed identity accepted direct mutation: %s", query)
		}
	}
}

func TestWorkspaceCaptureLedgerSerializesCompetingSeals(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ref, _ := workspaceLedgerFixture(t, databases[0], pool, 1703)
	other := ref
	other.SHA256 = strings.Repeat("b", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- databases[0].SealCapture(ctx, ref) }()
	go func() { <-start; results <- databases[1].SealCapture(ctx, other) }()
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, execution.ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("competing seals: %d successes, %d conflicts", successes, conflicts)
	}
	loaded, found, err := databases[0].LoadCapture(ctx, ref.Request)
	if err != nil || !found || loaded != ref && loaded != other {
		t.Fatalf("winning seal = %#v, %t, %v", loaded, found, err)
	}
	// Purpose and arbitrary operation text each define distinct identities.
	for _, request := range []execution.CaptureRequest{
		{Workspace: ref.Request.Workspace, Purpose: execution.TerminalCapture, OperationID: ref.Request.OperationID},
		{Workspace: ref.Request.Workspace, Purpose: execution.PublicationCapture, OperationID: strings.Repeat("x", 256)},
	} {
		separate := execution.CaptureRef{Request: request, SHA256: ref.SHA256}
		if err := databases[0].SealCapture(ctx, separate); err != nil {
			t.Fatal(err)
		}
		if got, found, err := databases[1].LoadCapture(ctx, request); err != nil || !found || got != separate {
			t.Fatalf("separate capture = %#v, %t, %v", got, found, err)
		}
	}
}

func TestWorkspaceCaptureLedgerRejectsMalformedRequests(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ref, _ := workspaceLedgerFixture(t, databases[0], pool, 1704)
	for _, edit := range []func(*execution.CaptureRef){
		func(ref *execution.CaptureRef) { ref.SHA256 = "invalid" },
		func(ref *execution.CaptureRef) { ref.SHA256 = strings.Repeat("A", 64) },
		func(ref *execution.CaptureRef) { ref.Request.OperationID = "" },
		func(ref *execution.CaptureRef) { ref.Request.OperationID = strings.Repeat("x", 257) },
		func(ref *execution.CaptureRef) { ref.Request.OperationID = "bad\x00operation" },
		func(ref *execution.CaptureRef) { ref.Request.OperationID = string([]byte{0xff}) },
		func(ref *execution.CaptureRef) { ref.Request.Purpose = "other" },
		func(ref *execution.CaptureRef) { ref.Request.Workspace.Key.ExecutionEpoch = 0 },
		func(ref *execution.CaptureRef) { ref.Request.Workspace.Key.AgentTurnID = "not a UUID" },
		func(ref *execution.CaptureRef) { ref.Request.Workspace.Key.RuntimeProfileName = "" },
		func(ref *execution.CaptureRef) { ref.Request.Workspace.Seed = string([]byte{0xff}) },
	} {
		wrong := ref
		edit(&wrong)
		if err := databases[0].SealCapture(context.Background(), wrong); !errors.Is(err, execution.ErrInvalidSpec) {
			t.Fatalf("malformed seal = %v, want invalid specification", err)
		}
		if wrong.Request != ref.Request {
			if _, _, err := databases[0].LoadCapture(context.Background(), wrong.Request); !errors.Is(err, execution.ErrInvalidSpec) {
				t.Fatalf("malformed load = %v, want invalid specification", err)
			}
		}
	}
}

func workspaceLedgerFixture(t *testing.T, database *store.Store, pool *pgxpool.Pool, number int) (execution.CaptureRef, store.AgentTurnLease) {
	t.Helper()
	ctx := context.Background()
	fixture := seedAgentSession(t, pool, number)
	turn, err := prepareFixtureAgentTurn(t, database, pool, ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, turn), turn.ControlRevision, "capture-ledger", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	current, err := database.GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	key := execution.Key{AssignmentID: current.Participant.ID, AgentSessionID: current.Session.ID, AgentTurnID: current.Turn.ID,
		ExecutionEpoch: current.Turn.ExecutionEpoch, RuntimeProfileName: current.Participant.RuntimeProfileName,
		RuntimeProfileVersion: current.Participant.RuntimeProfileVersion}
	return execution.CaptureRef{Request: execution.CaptureRequest{
		Workspace:   execution.WorkspaceRef{Key: key, Seed: "seed revision"},
		OperationID: "github:publish:operation with spaces/and:punctuation", Purpose: execution.PublicationCapture,
	}, SHA256: strings.Repeat("a", 64)}, lease
}

func workspaceLedgerCoordinationState(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
    'workflows', (SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM workflows w),
    'turns', (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM agent_turns t),
    'sessions', (SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM agent_sessions s),
    'jobs', (SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM jobs j))::text`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

type ledgerIntegrationCaptureSource struct {
	archive []byte
	calls   int
	fence   func(context.Context, func(context.Context) error) error
}

func (source *ledgerIntegrationCaptureSource) ReadWorkspace(ctx context.Context, request execution.CaptureRequest, consume func(io.Reader) error) error {
	source.calls++
	operation := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return consume(bytes.NewReader(source.archive))
	}
	if source.fence != nil {
		return source.fence(ctx, operation)
	}
	return operation(ctx)
}

func workspaceLedgerArchive(t *testing.T, content string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "workspace/evidence.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
