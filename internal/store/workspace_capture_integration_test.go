//go:build integration

package store_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/gitremote"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestTerminalCaptureFixtureRetainsOriginalWorkspaceEvidence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx := context.Background()
	_, lease, _ := prepareOpenSettlementTurn(t, databases[0], pool, ctx, 1081, workflow.RoleDeveloper, "")
	current, err := databases[0].GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := execution.Key{AssignmentID: current.Assignment.ID, AgentSessionID: current.Session.ID, AgentTurnID: current.Turn.ID,
		ExecutionEpoch: current.Turn.ExecutionEpoch, RuntimeProfileName: current.Assignment.RuntimeProfileName, RuntimeProfileVersion: current.Assignment.RuntimeProfileVersion}
	for _, name := range []string{"clean", "dirty tracked", "untracked", "unpublished commit"} {
		t.Run(name, func(t *testing.T) {
			paths := committedPublicationPaths(t)
			publishedHead, err := exec.Command("git", "-C", paths.Publication, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			publishedRevision := strings.TrimSpace(string(publishedHead))
			if name != "clean" {
				file := "tracked.txt"
				if name == "untracked" {
					file = "late.txt"
				}
				if err := os.WriteFile(filepath.Join(paths.Workspace, file), []byte("late changes"), 0o644); err != nil {
					t.Fatal(err)
				}
				if name == "unpublished commit" {
					for _, arguments := range [][]string{{"add", "tracked.txt"}, {"commit", "--quiet", "-m", "Unpublished change"}} {
						command := exec.Command("git", arguments...)
						command.Dir = paths.Workspace
						if output, err := command.CombinedOutput(); err != nil {
							t.Fatalf("git %v: %v: %s", arguments, err, output)
						}
					}
				}
			}
			retained, ref := integrationTerminalWorkspace(t, pool, lease, paths, publishedRevision, "installation-token")
			if ref.Request.Workspace.Key != wantKey {
				t.Fatalf("capture lacks persisted execution binding: %#v", ref.Request)
			}
			if err := os.RemoveAll(paths.Workspace); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(paths.Workspace, "replacement.txt"), []byte("new execution"), 0o644); err != nil {
				t.Fatal(err)
			}
			cache, err := retained.Paths(wantKey.AssignmentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(cache.Publication); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(cache.Publication, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache.Publication, "replacement.txt"), []byte("unrelated cache"), 0o644); err != nil {
				t.Fatal(err)
			}
			matching, err := retained.CaptureTreesEqual(ctx, ref, workspace.PublicationObservation{
				RepositoryURL: retained.authorizedURL, Credential: "installation-token", Revision: publishedRevision,
			})
			if err != nil || matching != (name == "clean") {
				t.Fatalf("retained original comparison = %t, %v", matching, err)
			}
		})
	}
}

func integrationWorkspaceOptions(t *testing.T) workspace.Options {
	t.Helper()
	root := t.TempDir()
	options := workspace.Options{WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"),
		MiseRoot: filepath.Join(root, "mise"), CaptureRoot: filepath.Join(root, "captures")}
	for _, path := range []string{options.WorkspaceRoot, options.PublicationRoot, options.MiseRoot, options.CaptureRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return options
}

// Reviewer and blocked fixtures need a real inspector but no Developer capture.
func newIntegrationOutcomeWorkspace(t *testing.T) *workspace.Lifecycle {
	t.Helper()
	lifecycle, err := workspace.New(integrationWorkspaceOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle
}

// Recovery no longer owns a live Turn lease. Read the persisted execution key
// directly, including the Participant's immutable Runtime Profile binding.
func integrationCaptureKey(t *testing.T, pool *pgxpool.Pool, lease store.AgentTurnLease) execution.Key {
	t.Helper()
	var key execution.Key
	err := pool.QueryRow(context.Background(), `
SELECT participant.id::text, turn.agent_session_id::text, turn.id::text, turn.execution_epoch,
       participant.runtime_profile_name, participant.runtime_profile_version
FROM agent_turns AS turn JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS participant ON participant.id = session.agent_assignment_id
WHERE turn.id = $1 AND turn.execution_epoch = $2 AND participant.id = $3 AND turn.agent_session_id = $4
  AND session.runtime_profile_name = participant.runtime_profile_name
  AND session.runtime_profile_version = participant.runtime_profile_version`,
		lease.ID, lease.ExecutionEpoch, lease.AgentAssignmentID, lease.AgentSessionID).Scan(
		&key.AssignmentID, &key.AgentSessionID, &key.AgentTurnID, &key.ExecutionEpoch, &key.RuntimeProfileName, &key.RuntimeProfileVersion)
	if err != nil {
		t.Fatalf("read persisted capture key: %v", err)
	}
	return key
}

func integrationTerminalWorkspace(t *testing.T, pool *pgxpool.Pool, lease store.AgentTurnLease, paths workspace.Paths, expectedRevision, expectedCredential string) (*integrationCaptureWorkspace, execution.CaptureRef) {
	t.Helper()
	key := integrationCaptureKey(t, pool, lease)
	request := execution.CaptureRequest{Workspace: execution.WorkspaceRef{Key: key}, OperationID: key.AgentTurnID, Purpose: execution.TerminalCapture}
	// Freeze fixture bytes before supplying a source. The source cannot observe
	// subsequent edits, replacement, or removal of the original live checkout.
	archive := frozenIntegrationWorkspaceArchive(t, paths.Workspace)
	options := integrationWorkspaceOptions(t)
	options.CaptureSource = frozenIntegrationWorkspaceSource{request: request, archive: archive}
	lifecycle, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := lifecycle.Paths(key.AssignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(trusted.Publication, os.DirFS(paths.Publication)); err != nil {
		t.Fatal(err)
	}
	// Keep the authoritative repository outside disposable workspace/cache roots.
	remote := filepath.Join(t.TempDir(), "authoritative.git")
	command := exec.Command("git", "clone", "--bare", "--no-hardlinks", paths.Publication, remote)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone authoritative fixture: %v: %s", err, output)
	}
	command = exec.Command("git", "-C", remote, "rev-parse", "HEAD")
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var owner, repository string
	if err := pool.QueryRow(context.Background(), `SELECT repository_owner, repository_name FROM workflows WHERE id = $1`, lease.JobLease.WorkflowID).Scan(&owner, &repository); err != nil {
		t.Fatal(err)
	}
	base, err := gitremote.ParseBaseURL("")
	if err != nil {
		t.Fatal(err)
	}
	authorizedURL, err := base.RepositoryURL(owner, repository)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := lifecycle.Capture(context.Background(), request)
	if err != nil {
		t.Fatalf("seal terminal fixture: %v", err)
	}
	reader, err := lifecycle.OpenCapture(context.Background(), ref)
	if err != nil {
		t.Fatalf("open sealed terminal fixture: %v", err)
	}
	_, readErr := io.Copy(io.Discard, reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		t.Fatal(err)
	}
	// A new service with no source must recover the same persisted reference.
	options.CaptureSource = nil
	retained, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := retained.TerminalCapture(context.Background(), key)
	if err != nil || reopened != ref {
		t.Fatalf("retained terminal fixture = %#v, %v; want %#v", reopened, err, ref)
	}
	return &integrationCaptureWorkspace{Lifecycle: retained, authorizedURL: authorizedURL, expectedRevision: expectedRevision,
		expectedCredential: expectedCredential, remote: remote, remoteRevision: strings.TrimSpace(string(head))}, ref
}

type integrationCaptureWorkspace struct {
	*workspace.Lifecycle
	authorizedURL, expectedRevision, expectedCredential string
	remote, remoteRevision                              string
}

func (fixture *integrationCaptureWorkspace) CaptureTreesEqual(ctx context.Context, ref execution.CaptureRef, observation workspace.PublicationObservation) (bool, error) {
	if observation.RepositoryURL != fixture.authorizedURL || observation.Credential != fixture.expectedCredential || observation.Revision != fixture.expectedRevision {
		return false, errors.New("unexpected authoritative publication observation")
	}
	// The pre-existing ledger/GitHub fixtures use synthetic revisions. Their
	// checked mapping ends here; trusted inspection still fetches real Git bytes.
	observation.RepositoryURL, observation.Revision = fixture.remote, fixture.remoteRevision
	return fixture.Lifecycle.CaptureTreesEqual(ctx, ref, observation)
}

type frozenIntegrationWorkspaceSource struct {
	request execution.CaptureRequest
	archive []byte
}

func (source frozenIntegrationWorkspaceSource) ReadWorkspace(ctx context.Context, request execution.CaptureRequest, consume func(io.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request != source.request {
		return errors.New("frozen fixture capture binding mismatch")
	}
	return consume(bytes.NewReader(source.archive))
}

func frozenIntegrationWorkspaceArchive(t *testing.T, directory string) []byte {
	t.Helper()
	var content bytes.Buffer
	writer := tar.NewWriter(&content)
	if err := writer.WriteHeader(&tar.Header{Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == directory {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		header.Name = "workspace/" + filepath.ToSlash(relative)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		return errors.Join(copyErr, file.Close())
	})
	if err := errors.Join(err, writer.Close()); err != nil {
		t.Fatalf("freeze workspace fixture: %v", err)
	}
	return content.Bytes()
}
