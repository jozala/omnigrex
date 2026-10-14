package workspace_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/workspace"
)

// frozenCaptureSource models a backend-owned snapshot, with no host path exposed
// to the consumer and all writers excluded for the complete callback.
type frozenCaptureSource struct {
	mu       sync.Mutex
	content  []byte
	requests []execution.CaptureRequest
	err      error
}

func (source *frozenCaptureSource) ReadWorkspace(ctx context.Context, request execution.CaptureRequest, consume func(io.Reader) error) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	source.requests = append(source.requests, request)
	if err := consume(bytes.NewReader(source.content)); err != nil {
		return err
	}
	return source.err
}

func (source *frozenCaptureSource) freeze(t *testing.T, directory string) {
	t.Helper()
	content, err := freezeCaptureDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	source.content = content
}

func freezeCaptureDirectory(directory string) ([]byte, error) {
	var content bytes.Buffer
	writer := tar.NewWriter(&content)
	err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(name)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		header.Name = "workspace"
		if relative != "." {
			header.Name += "/" + filepath.ToSlash(relative)
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		return errors.Join(copyErr, file.Close())
	})
	if err = errors.Join(err, writer.Close()); err != nil {
		return nil, err
	}
	return bytes.Clone(content.Bytes()), nil
}

type captureGitFixture struct {
	git       gitFixture
	lifecycle *workspace.Lifecycle
	options   workspace.Options
	paths     workspace.Paths
	source    *frozenCaptureSource
	request   execution.CaptureRequest
}

func newCaptureGitFixture(t *testing.T) captureGitFixture {
	t.Helper()
	return newCaptureGitFixtureWithObjectFormat(t, "sha1")
}

func newCaptureGitFixtureWithObjectFormat(t *testing.T, objectFormat string) captureGitFixture {
	t.Helper()
	fixture := newGitFixtureWithObjectFormat(t, objectFormat)
	root := t.TempDir()
	source := &frozenCaptureSource{}
	options := workspace.Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"),
		MiseRoot: filepath.Join(root, "mise"), CaptureRoot: filepath.Join(root, "captures"), CaptureSource: source,
	}
	lifecycle, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := lifecycle.PrepareWorkspace(context.Background(), workspace.Checkout{
		AssignmentID: assignmentID, RepositoryURL: fixture.remote, Revision: fixture.second, ExecutionEpoch: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Most cases model an existing publication service; the cold-start case
	// explicitly removes this directory to exercise first-use initialization.
	if err := os.MkdirAll(options.PublicationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	request := execution.CaptureRequest{
		Workspace: execution.WorkspaceRef{Key: execution.Key{
			AssignmentID: assignmentID, AgentSessionID: "12345678-1234-4234-8234-123456789abd",
			AgentTurnID: "12345678-1234-4234-8234-123456789abe", ExecutionEpoch: 7,
			RuntimeProfileName: "pi", RuntimeProfileVersion: "v1",
		}}, OperationID: "publish-operation", Purpose: execution.PublicationCapture,
	}
	return captureGitFixture{git: fixture, lifecycle: lifecycle, options: options, paths: paths, source: source, request: request}
}

func (fixture captureGitFixture) publication() workspace.Publication {
	return workspace.Publication{
		AssignmentID: assignmentID, RepositoryURL: fixture.git.remote, BaseRevision: fixture.git.second,
		Branch: "feature", DefaultBranch: "main", Capture: fixture.request,
		RecordProposedTip: func(context.Context, string) error { return nil },
	}
}

func (fixture captureGitFixture) commit(t *testing.T, content, message string) string {
	t.Helper()
	writeFile(t, filepath.Join(fixture.paths.Workspace, "tracked.txt"), content, 0o644)
	gitRun(t, fixture.paths.Workspace, "commit", "-am", message)
	return gitOutput(t, fixture.paths.Workspace, "rev-parse", "HEAD")
}

func captureContents(t *testing.T, lifecycle *workspace.Lifecycle, ref execution.CaptureRef) []byte {
	t.Helper()
	reader, err := lifecycle.OpenCapture(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err = errors.Join(err, reader.Close()); err != nil {
		t.Fatal(err)
	}
	if err := workspace.VerifyCapture(bytes.NewReader(content), ref); err != nil {
		t.Fatalf("stored artifact digest: %v", err)
	}
	return content
}

func TestCapturePublicationInitializesColdStorage(t *testing.T) {
	fixture := newCaptureGitFixture(t)
	if err := os.RemoveAll(fixture.options.PublicationRoot); err != nil {
		t.Fatal(err)
	}
	want := fixture.commit(t, "cold publication\n", "First captured publication")
	fixture.source.freeze(t, fixture.paths.Workspace)
	result, err := fixture.lifecycle.Publish(context.Background(), fixture.publication())
	if err != nil || result.Head != want || !result.Changed {
		t.Fatalf("first captured publication = %#v, %v", result, err)
	}
}

func TestCapturePublicationRequiresSourceAndValidScope(t *testing.T) {
	for _, name := range []string{"missing source", "wrong assignment", "terminal purpose", "invalid session"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCaptureGitFixture(t)
			fixture.commit(t, "candidate\n", "Candidate must require capture")
			fixture.source.freeze(t, fixture.paths.Workspace)
			publication := fixture.publication()
			publication.RecordProposedTip = func(context.Context, string) error {
				t.Error("invalid capture admission reached durable tip recording")
				return nil
			}
			lifecycle := fixture.lifecycle
			want := workspace.ErrInvalidCapture
			switch name {
			case "missing source":
				options := fixture.options
				options.CaptureSource = nil
				var err error
				lifecycle, err = workspace.New(options)
				if err != nil {
					t.Fatal(err)
				}
				want = execution.ErrUnsupportedRequirement
			case "wrong assignment":
				publication.Capture.Workspace.Key.AssignmentID = "12345678-1234-4234-8234-123456789abf"
			case "terminal purpose":
				publication.Capture.Purpose = execution.TerminalCapture
			case "invalid session":
				publication.Capture.Workspace.Key.AgentSessionID = ""
			}
			if _, err := lifecycle.Publish(context.Background(), publication); !errors.Is(err, want) {
				t.Fatalf("publication capture admission = %v, want %v", err, want)
			}
			if got := gitOutput(t, fixture.git.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
				t.Errorf("invalid capture admission pushed %s", got)
			}
		})
	}
}

func TestCapturePublicationPreservesExactMultiCommitMergeHistoryAfterSourceRemoval(t *testing.T) {
	fixture := newCaptureGitFixture(t)
	writeFile(t, filepath.Join(fixture.paths.Workspace, "developer.txt"), "first developer edit\n", 0o644)
	gitRun(t, fixture.paths.Workspace, "add", "developer.txt")
	gitRun(t, fixture.paths.Workspace, "commit", "-m", "First developer commit")
	first := gitOutput(t, fixture.paths.Workspace, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(fixture.paths.Workspace, "developer.txt"), "second developer edit\n", 0o644)
	gitRun(t, fixture.paths.Workspace, "commit", "-am", "Second developer commit")
	second := gitOutput(t, fixture.paths.Workspace, "rev-parse", "HEAD")
	main := commitFixtureChange(t, fixture.git.remote, fixture.git.second, "main", "main advance\n", "Advance main")
	gitRun(t, fixture.paths.Workspace, "fetch", "origin", "main")
	gitRun(t, fixture.paths.Workspace, "merge", "--no-ff", "--no-edit", "FETCH_HEAD")
	if err := os.Remove(filepath.Join(fixture.paths.Workspace, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.paths.Workspace, "bin", "tool"), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(fixture.paths.Workspace, ".gitignore"), "cache/\n", 0o644)
	if err := os.Symlink("bin/tool", filepath.Join(fixture.paths.Workspace, "tool")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, fixture.paths.Workspace, "add", "-A")
	gitRun(t, fixture.paths.Workspace, "commit", "-m", "Preserve executable and symbolic link")
	head := gitOutput(t, fixture.paths.Workspace, "rev-parse", "HEAD")
	merged := gitOutput(t, fixture.paths.Workspace, "rev-parse", "HEAD~1")
	wantObjects := make(map[string]string)
	for _, commit := range []string{first, second, merged, head} {
		wantObjects[commit] = gitOutput(t, fixture.paths.Workspace, "cat-file", "commit", commit)
	}
	writeFile(t, filepath.Join(fixture.paths.Workspace, "cache", "private.txt"), "ignored but retained in capture\n", 0o600)
	fixture.source.freeze(t, fixture.paths.Workspace)
	ref, err := fixture.lifecycle.Capture(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	content := captureContents(t, fixture.lifecycle, ref)
	archive := tar.NewReader(bytes.NewReader(content))
	retainedIgnored := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "cache/private.txt" {
			body, err := io.ReadAll(archive)
			if err != nil || string(body) != "ignored but retained in capture\n" {
				t.Fatalf("ignored inventory = %q, %v", body, err)
			}
			retainedIgnored = true
		}
	}
	if !retainedIgnored {
		t.Fatal("capture omitted ignored working-tree inventory")
	}
	if err := os.RemoveAll(fixture.paths.Workspace); err != nil {
		t.Fatal(err)
	}
	publication := fixture.publication()
	publication.RecordProposedTip = func(_ context.Context, proposed string) error {
		if proposed != head {
			t.Errorf("recorded %s, want exact captured tip %s", proposed, head)
		}
		return nil
	}
	result, err := fixture.lifecycle.Publish(context.Background(), publication)
	if err != nil || !result.Changed || result.Head != head {
		t.Fatalf("publication without source checkout = %#v, %v", result, err)
	}
	for commit, want := range wantObjects {
		if got := gitOutput(t, fixture.git.remote, "cat-file", "commit", commit); got != want {
			t.Errorf("published commit %s was rewritten:\n%s\nwant:\n%s", commit, got, want)
		}
	}
	if got := gitOutput(t, fixture.git.remote, "rev-list", "--parents", "-n", "1", merged); got != merged+" "+second+" "+main {
		t.Errorf("merge parents = %s", got)
	}
	if got := gitOutput(t, fixture.git.remote, "ls-tree", "-r", head); !strings.Contains(got, "100755 blob") || !strings.Contains(got, "120000 blob") {
		t.Errorf("published modes = %s", got)
	}
	if got := gitOutput(t, fixture.git.remote, "show", head+":tool"); got != "bin/tool" {
		t.Errorf("published symlink target = %q", got)
	}
	if got := gitOutput(t, fixture.git.remote, "ls-tree", "-r", "--name-only", head); strings.Contains(got, "tracked.txt") || strings.Contains(got, "cache/") {
		t.Errorf("deleted or ignored file published: %s", got)
	}
	if equal, err := fixture.lifecycle.CaptureTreesEqual(context.Background(), ref, workspace.PublicationObservation{
		RepositoryURL: fixture.git.remote, Revision: result.Head,
	}); err != nil || !equal {
		t.Fatalf("sealed evidence comparison = %t, %v", equal, err)
	}
}

func TestCapturePublicationPreservesSHA256MultiCommitHistoryAndNeutralizesPoisonedConfig(t *testing.T) {
	for _, headKind := range []string{"detached", "loose ref", "packed ref"} {
		t.Run(headKind, func(t *testing.T) {
			ctx := context.Background()
			fixture := newCaptureGitFixtureWithObjectFormat(t, "sha256")
			first := fixture.commit(t, "first SHA256 developer commit\n", "First SHA256 developer commit")
			second := fixture.commit(t, "second SHA256 developer commit\n", "Second SHA256 developer commit")
			wantObjects := make(map[string]string)
			for _, revision := range []string{fixture.git.second, first, second} {
				if len(revision) != 64 {
					t.Fatalf("SHA256 fixture produced non-64-character object identity: %s", revision)
				}
				wantObjects[revision] = gitOutput(t, fixture.paths.Workspace, "cat-file", "commit", revision)
			}
			if headKind != "detached" {
				gitRun(t, fixture.paths.Workspace, "checkout", "-b", "captured-feature")
				if headKind == "packed ref" {
					gitRun(t, fixture.paths.Workspace, "pack-refs", "--all", "--prune")
					if _, err := os.Stat(filepath.Join(fixture.paths.Workspace, ".git", "refs", "heads", "captured-feature")); !os.IsNotExist(err) {
						t.Fatalf("SHA256 symbolic HEAD was not packed: %v", err)
					}
				}
			}
			marker := filepath.Join(t.TempDir(), "agent-config-executed")
			command := filepath.Join(t.TempDir(), "agent-command")
			writeExecutable(t, command, "#!/bin/sh\ntouch "+quoted(marker)+"\nexit 77\n")
			poisonConfig := filepath.Join(t.TempDir(), "poison.config")
			writeFile(t, poisonConfig,
				"[core]\nworktree = "+quoted(t.TempDir())+"\nfsmonitor = "+quoted(command)+"\nhooksPath = "+quoted(filepath.Dir(command))+"\n"+
					"[extensions]\nobjectFormat = sha1\n[uploadpack]\npackObjectsHook = "+quoted(command)+"\n[alias]\ninspect = !"+command+"\n", 0o600)
			gitRun(t, fixture.paths.Workspace, "config", "include.path", poisonConfig)
			writeFile(t, filepath.Join(fixture.paths.Workspace, ".git", "config.worktree"), "[include]\npath = "+quoted(poisonConfig)+"\n", 0o600)
			writeFile(t, filepath.Join(fixture.paths.Workspace, ".git", "hooks", "post-checkout"), "#!/bin/sh\ntouch "+quoted(marker)+"\nexit 77\n", 0o755)
			fixture.source.freeze(t, fixture.paths.Workspace)
			ref, err := fixture.lifecycle.Capture(ctx, fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(fixture.paths.Workspace); err != nil {
				t.Fatal(err)
			}
			publication := fixture.publication()
			recorded := ""
			publication.RecordProposedTip = func(_ context.Context, head string) error {
				recorded = head
				return nil
			}
			result, err := fixture.lifecycle.Publish(ctx, publication)
			if err != nil || !result.Changed || result.Head != second || recorded != second {
				t.Fatalf("SHA256 captured publication = %#v, %v, recorded %s", result, err, recorded)
			}
			if got := gitOutput(t, fixture.git.remote, "rev-parse", "refs/heads/feature"); got != second {
				t.Errorf("authoritative SHA256 publication = %s, want %s", got, second)
			}
			for revision, want := range wantObjects {
				if got := gitOutput(t, fixture.git.remote, "cat-file", "commit", revision); got != want {
					t.Errorf("SHA256 commit %s rewritten:\n%s\nwant:\n%s", revision, got, want)
				}
			}
			if got := gitOutput(t, fixture.git.remote, "rev-list", "--parents", "-n", "1", second); got != second+" "+first {
				t.Errorf("SHA256 second commit parent = %s", got)
			}
			if got := gitOutput(t, fixture.git.remote, "rev-list", "--parents", "-n", "1", first); got != first+" "+fixture.git.second {
				t.Errorf("SHA256 first commit parent = %s", got)
			}
			if equal, err := fixture.lifecycle.CaptureTreesEqual(ctx, ref, workspace.PublicationObservation{
				RepositoryURL: fixture.git.remote, Revision: second,
			}); err != nil || !equal {
				t.Fatalf("SHA256 retained capture corroboration = %t, %v", equal, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("captured include or hook executed an agent command: %v", err)
			}
		})
	}
}

func TestCapturePublicationRejectsDirtyInventory(t *testing.T) {
	for _, name := range []string{"unstaged", "staged", "untracked", "missing tracked", "assume unchanged", "skip worktree"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCaptureGitFixture(t)
			fixture.commit(t, "committed\n", "Valid candidate")
			switch name {
			case "untracked":
				writeFile(t, filepath.Join(fixture.paths.Workspace, "new.txt"), "untracked\n", 0o644)
			case "missing tracked":
				if err := os.Remove(filepath.Join(fixture.paths.Workspace, "tracked.txt")); err != nil {
					t.Fatal(err)
				}
			default:
				if name == "assume unchanged" || name == "skip worktree" {
					flag := "--assume-unchanged"
					if name == "skip worktree" {
						flag = "--skip-worktree"
					}
					gitRun(t, fixture.paths.Workspace, "update-index", flag, "tracked.txt")
				}
				writeFile(t, filepath.Join(fixture.paths.Workspace, "tracked.txt"), "dirty\n", 0o644)
				if name == "staged" {
					gitRun(t, fixture.paths.Workspace, "add", "tracked.txt")
				}
			}
			fixture.source.freeze(t, fixture.paths.Workspace)
			publication := fixture.publication()
			publication.RecordProposedTip = func(context.Context, string) error {
				t.Error("dirty capture reached durable tip recording")
				return nil
			}
			_, err := fixture.lifecycle.Publish(context.Background(), publication)
			want := workspace.ErrUncommittedWorkspace
			if name == "assume unchanged" || name == "skip worktree" {
				want = workspace.ErrTreeMismatch
			}
			if !errors.Is(err, want) {
				t.Fatalf("dirty captured publication = %v, want %v", err, want)
			}
			if got := gitOutput(t, fixture.git.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
				t.Errorf("dirty capture pushed %s", got)
			}
		})
	}
}

func TestCapturePublicationRejectsInvalidHistoryAndMetadata(t *testing.T) {
	for _, name := range []string{"rewritten first parent", "nondefault merge", "grafts", "shallow", "replace", "packed replace", "alternates", "http alternates", "commondir"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCaptureGitFixture(t)
			switch name {
			case "rewritten first parent":
				gitRun(t, fixture.paths.Workspace, "checkout", "--detach", fixture.git.first)
				fixture.commit(t, "rewritten\n", "Rewrite base")
			case "nondefault merge":
				writeFile(t, filepath.Join(fixture.paths.Workspace, "developer.txt"), "developer\n", 0o644)
				gitRun(t, fixture.paths.Workspace, "add", "developer.txt")
				gitRun(t, fixture.paths.Workspace, "commit", "-m", "Developer commit")
				commitFixtureChange(t, fixture.git.remote, fixture.git.second, "other", "other\n", "Untrusted merge parent")
				gitRun(t, fixture.paths.Workspace, "fetch", "origin", "other")
				gitRun(t, fixture.paths.Workspace, "merge", "--no-ff", "--no-edit", "FETCH_HEAD")
			case "replace", "packed replace":
				gitRun(t, fixture.paths.Workspace, "replace", "HEAD", fixture.git.first)
				if name == "packed replace" {
					gitRun(t, fixture.paths.Workspace, "pack-refs", "--all", "--prune")
				}
			default:
				metadata := map[string]string{"grafts": "info/grafts", "shallow": "shallow", "alternates": "objects/info/alternates", "http alternates": "objects/info/http-alternates", "commondir": "commondir"}[name]
				writeFile(t, filepath.Join(fixture.paths.Workspace, ".git", metadata), fixture.git.second+"\n", 0o644)
			}
			fixture.source.freeze(t, fixture.paths.Workspace)
			publication := fixture.publication()
			publication.RecordProposedTip = func(context.Context, string) error {
				t.Error("invalid history reached durable tip recording")
				return nil
			}
			if _, err := fixture.lifecycle.Publish(context.Background(), publication); !errors.Is(err, workspace.ErrInvalidPublicationHistory) {
				t.Fatalf("captured invalid history = %v", err)
			}
			if got := gitOutput(t, fixture.git.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
				t.Errorf("invalid history pushed %s", got)
			}
		})
	}
}

func TestCapturePublicationNeutralizesAgentGitConfigurationAndHooks(t *testing.T) {
	fixture := newCaptureGitFixture(t)
	want := fixture.commit(t, "safe publication\n", "Candidate")
	marker := filepath.Join(t.TempDir(), "agent-command-executed")
	hook := filepath.Join(t.TempDir(), "agent-command")
	writeExecutable(t, hook, "#!/bin/sh\ntouch "+quoted(marker)+"\nexit 7\n")
	gitRun(t, fixture.paths.Workspace, "config", "uploadpack.packObjectsHook", hook)
	gitRun(t, fixture.paths.Workspace, "config", "core.fsmonitor", hook)
	gitRun(t, fixture.paths.Workspace, "config", "core.worktree", t.TempDir())
	writeFile(t, filepath.Join(fixture.paths.Workspace, ".git", "hooks", "post-checkout"), "#!/bin/sh\ntouch "+quoted(marker)+"\n", 0o755)
	fixture.source.freeze(t, fixture.paths.Workspace)
	if result, err := fixture.lifecycle.Publish(context.Background(), fixture.publication()); err != nil || result.Head != want {
		t.Fatalf("publish poisoned metadata = %#v, %v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("captured agent command executed: %v", err)
	}
}

func TestCapturePublicationChecksRemoteHeadAfterRecording(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new branch"
		if existing {
			name = "existing branch"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newCaptureGitFixture(t)
			competing := commitFixtureChange(t, fixture.git.remote, fixture.git.second, "competing", "other\n", "Other publisher")
			proposed := fixture.commit(t, "candidate\n", "Candidate")
			fixture.source.freeze(t, fixture.paths.Workspace)
			publication := fixture.publication()
			if existing {
				gitRun(t, fixture.git.remote, "update-ref", "refs/heads/feature", fixture.git.second)
				publication.ExpectedOldHead = fixture.git.second
			}
			recorded := ""
			publication.RecordProposedTip = func(_ context.Context, head string) error {
				recorded = head
				gitRun(t, fixture.git.remote, "update-ref", "refs/heads/feature", competing)
				return nil
			}
			_, err := fixture.lifecycle.Publish(context.Background(), publication)
			var mismatch *workspace.UnexpectedHeadError
			if !errors.As(err, &mismatch) || mismatch.Expected != publication.ExpectedOldHead || mismatch.Actual != competing || recorded != proposed {
				t.Fatalf("remote race = %v, recorded %s", err, recorded)
			}
			if got := gitOutput(t, fixture.git.remote, "rev-parse", "refs/heads/feature"); got != competing {
				t.Fatalf("competing head overwritten: %s", got)
			}
		})
	}
}

func TestCapturePublicationRetryUsesSealedEvidenceAfterRestartAndSourceChanges(t *testing.T) {
	fixture := newCaptureGitFixture(t)
	want := fixture.commit(t, "original candidate\n", "Original candidate")
	fixture.source.freeze(t, fixture.paths.Workspace)
	publication := fixture.publication()
	recordFailure := errors.New("durable recording unavailable")
	publication.RecordProposedTip = func(context.Context, string) error { return recordFailure }
	if _, err := fixture.lifecycle.Publish(context.Background(), publication); !errors.Is(err, recordFailure) {
		t.Fatal(err)
	}
	ref, err := fixture.lifecycle.Capture(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	original := captureContents(t, fixture.lifecycle, ref)
	fixture.commit(t, "later candidate\n", "Later candidate must not replace evidence")
	fixture.source.freeze(t, fixture.paths.Workspace)
	restarted, err := workspace.New(fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := restarted.Capture(context.Background(), fixture.request)
	if err != nil || retried != ref || !bytes.Equal(original, captureContents(t, restarted, retried)) {
		t.Fatalf("sealed retry = %#v, %v", retried, err)
	}
	publication.RecordProposedTip = func(_ context.Context, head string) error {
		if head != want {
			t.Errorf("retry recorded later tip %s", head)
		}
		return nil
	}
	result, err := restarted.Publish(context.Background(), publication)
	if err != nil || result.Head != want {
		t.Fatalf("restart publication = %#v, %v", result, err)
	}
	if got := gitOutput(t, fixture.git.remote, "rev-parse", "refs/heads/feature"); got != want {
		t.Errorf("retry pushed later workspace: %s", got)
	}
	if len(fixture.source.requests) != 1 {
		t.Errorf("sealed operation re-observed source %d times", len(fixture.source.requests))
	}
}

func TestCaptureTerminalEvidenceDetectsLateChangesAndSurvivesRemovalAndRestart(t *testing.T) {
	for _, change := range []string{"clean", "unstaged", "staged", "untracked", "new commit"} {
		t.Run(change, func(t *testing.T) {
			fixture := newCaptureGitFixture(t)
			fixture.commit(t, "published\n", "Published candidate")
			fixture.source.freeze(t, fixture.paths.Workspace)
			result, err := fixture.lifecycle.Publish(context.Background(), fixture.publication())
			if err != nil {
				t.Fatal(err)
			}
			observation := workspace.PublicationObservation{RepositoryURL: fixture.git.remote, Revision: result.Head}
			publicationRef, err := fixture.lifecycle.Capture(context.Background(), fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "unstaged", "staged":
				writeFile(t, filepath.Join(fixture.paths.Workspace, "tracked.txt"), "late edit\n", 0o644)
				if change == "staged" {
					gitRun(t, fixture.paths.Workspace, "add", "tracked.txt")
				}
			case "untracked":
				writeFile(t, filepath.Join(fixture.paths.Workspace, "late.txt"), "late\n", 0o644)
			case "new commit":
				fixture.commit(t, "unpublished commit\n", "Late commit")
			}
			fixture.source.freeze(t, fixture.paths.Workspace)
			terminal := fixture.request
			terminal.Purpose, terminal.OperationID = execution.TerminalCapture, terminal.Workspace.Key.AgentTurnID
			ref, err := fixture.lifecycle.Capture(context.Background(), terminal)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Request.Purpose != execution.TerminalCapture || ref.Request == publicationRef.Request {
				t.Fatal("terminal capture reused publication identity")
			}
			if err := os.RemoveAll(fixture.paths.Workspace); err != nil {
				t.Fatal(err)
			}
			restarted, err := workspace.New(fixture.options)
			if err != nil {
				t.Fatal(err)
			}
			retained, err := restarted.TerminalCapture(context.Background(), terminal.Workspace.Key)
			if err != nil || retained != ref {
				t.Fatalf("retained terminal evidence = %#v, %v", retained, err)
			}
			captureContents(t, restarted, retained)
			equal, err := restarted.CaptureTreesEqual(context.Background(), retained, observation)
			if err != nil || equal != (change == "clean") {
				t.Fatalf("late %s corroboration = %t, %v", change, equal, err)
			}
			if equal, err := restarted.CaptureTreesEqual(context.Background(), publicationRef, observation); err != nil || !equal {
				t.Fatalf("earlier publication evidence changed = %t, %v", equal, err)
			}
		})
	}
}

func TestCaptureTreesEqualUsesPinnedAuthoritativeRevisionWithoutLocalDirectories(t *testing.T) {
	ctx := context.Background()
	fixture := newCaptureGitFixture(t)
	fixture.commit(t, "published original\n", "Original publication")
	fixture.source.freeze(t, fixture.paths.Workspace)
	result, err := fixture.lifecycle.Publish(ctx, fixture.publication())
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, fixture.git.remote, "rev-parse", "refs/heads/feature"); got != result.Head {
		t.Fatalf("authoritative publication = %s, want %s", got, result.Head)
	}
	publicationRef, err := fixture.lifecycle.Capture(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	wantArtifact := captureContents(t, fixture.lifecycle, publicationRef)
	lateHead := fixture.commit(t, "late unpublished change\n", "Unpublished terminal commit")
	fixture.source.freeze(t, fixture.paths.Workspace)
	terminalRequest := fixture.request
	terminalRequest.Purpose = execution.TerminalCapture
	terminalRequest.OperationID = terminalRequest.Workspace.Key.AgentTurnID
	terminalRef, err := fixture.lifecycle.Capture(ctx, terminalRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{fixture.options.WorkspaceRoot, fixture.options.PublicationRoot} {
		if err := os.RemoveAll(directory); err != nil {
			t.Fatal(err)
		}
	}
	options := fixture.options
	options.CaptureSource = nil
	restarted, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := captureContents(t, restarted, publicationRef); !bytes.Equal(got, wantArtifact) {
		t.Fatal("reopened retained artifact changed after removing local directories")
	}
	pinned := workspace.PublicationObservation{RepositoryURL: fixture.git.remote, Revision: result.Head}
	assertComparison := func(ref execution.CaptureRef, observation workspace.PublicationObservation, want bool) {
		t.Helper()
		equal, err := restarted.CaptureTreesEqual(ctx, ref, observation)
		if err != nil || equal != want {
			t.Fatalf("compare retained capture with authoritative %s = %t, %v, want %t", observation.Revision, equal, err, want)
		}
	}
	assertComparison(publicationRef, pinned, true)
	assertComparison(terminalRef, pinned, false)
	laterRemote := commitFixtureChange(t, fixture.git.remote, result.Head, "feature", "later remote publication\n", "Advance authoritative branch")
	if laterRemote == result.Head || laterRemote == lateHead {
		t.Fatal("remote advance must differ from both original publication and unpublished terminal commit")
	}
	if got := gitOutput(t, fixture.git.remote, "rev-list", "--all"); strings.Contains(got, lateHead) {
		t.Fatal("terminal commit unexpectedly became authoritative")
	}
	// Every comparison must work from retained evidence and a fresh trusted
	// clone, including after the earlier private inspection checkout is removed.
	if err := os.RemoveAll(fixture.options.PublicationRoot); err != nil {
		t.Fatal(err)
	}
	assertComparison(publicationRef, pinned, true)
	assertComparison(terminalRef, pinned, false)
	assertComparison(publicationRef, workspace.PublicationObservation{RepositoryURL: fixture.git.remote, Revision: laterRemote}, false)
}

func TestCaptureBackendFailureDoesNotSealOrPush(t *testing.T) {
	fixture := newCaptureGitFixture(t)
	want := fixture.commit(t, "candidate\n", "Candidate")
	fixture.source.freeze(t, fixture.paths.Workspace)
	failure := errors.New("environment could not resume after capture")
	fixture.source.err = failure
	publication := fixture.publication()
	publication.RecordProposedTip = func(context.Context, string) error {
		t.Error("failed backend capture reached durable recording")
		return nil
	}
	if _, err := fixture.lifecycle.Publish(context.Background(), publication); !errors.Is(err, failure) {
		t.Fatalf("backend failure = %v", err)
	}
	if got := gitOutput(t, fixture.git.remote, "for-each-ref", "--format=%(objectname)", "refs/heads/feature"); got != "" {
		t.Fatalf("failed capture pushed %s", got)
	}
	fixture.source.err = nil
	publication.RecordProposedTip = func(context.Context, string) error { return nil }
	if result, err := fixture.lifecycle.Publish(context.Background(), publication); err != nil || result.Head != want {
		t.Fatalf("retry after failed capture = %#v, %v", result, err)
	}
	if len(fixture.source.requests) != 2 {
		t.Fatalf("failed capture incorrectly sealed: source calls = %d", len(fixture.source.requests))
	}
}

func TestCaptureRetentionCollectsOnlyTheExplicitParticipantAfterSourceRemoval(t *testing.T) {
	ctx := context.Background()
	fixture := newCaptureGitFixture(t)
	fixture.source.freeze(t, fixture.paths.Workspace)
	publication, err := fixture.lifecycle.Capture(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	terminalRequest := fixture.request
	terminalRequest.Purpose = execution.TerminalCapture
	terminalRequest.OperationID = terminalRequest.Workspace.Key.AgentTurnID
	terminal, err := fixture.lifecycle.Capture(ctx, terminalRequest)
	if err != nil {
		t.Fatal(err)
	}
	const otherParticipant = "12345678-1234-4234-8234-123456789abf"
	otherPaths, err := fixture.lifecycle.PrepareWorkspace(ctx, workspace.Checkout{
		AssignmentID: otherParticipant, RepositoryURL: fixture.git.remote, Revision: fixture.git.first, ExecutionEpoch: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.source.freeze(t, otherPaths.Workspace)
	otherRequest := terminalRequest
	otherRequest.Workspace.Key.AssignmentID = otherParticipant
	otherRequest.Workspace.Key.AgentSessionID = "12345678-1234-4234-8234-123456789ac0"
	otherRequest.Workspace.Key.AgentTurnID = "12345678-1234-4234-8234-123456789ac1"
	otherRequest.Workspace.Key.ExecutionEpoch = 8
	otherRequest.OperationID = otherRequest.Workspace.Key.AgentTurnID
	other, err := fixture.lifecycle.Capture(ctx, otherRequest)
	if err != nil {
		t.Fatal(err)
	}
	wantContents := make(map[execution.CaptureRef][]byte)
	for _, ref := range []execution.CaptureRef{publication, terminal, other} {
		wantContents[ref] = captureContents(t, fixture.lifecycle, ref)
	}
	for _, participant := range []string{assignmentID, otherParticipant} {
		if err := fixture.lifecycle.DiscardWorkspace(participant); err != nil {
			t.Fatal(err)
		}
	}
	options := fixture.options
	options.CaptureSource = nil
	restarted, err := workspace.New(options)
	if err != nil {
		t.Fatal(err)
	}
	assertRetained := func(ref execution.CaptureRef) {
		t.Helper()
		if got := captureContents(t, restarted, ref); !bytes.Equal(got, wantContents[ref]) {
			t.Fatalf("retained capture changed for Participant %s", ref.Request.Workspace.Key.AssignmentID)
		}
	}
	// Turn scratch cleanup and live-workspace removal cannot collect evidence
	// still protected by the Participant's retention lifetime.
	if err := restarted.CleanupTurnPaths(ctx, assignmentID, terminalRequest.Workspace.Key.AgentTurnID, nil); err != nil {
		t.Fatal(err)
	}
	for ref := range wantContents {
		assertRetained(ref)
	}
	for _, invalid := range []string{"", "../escape", "not-a-participant"} {
		if err := restarted.CleanupAssignmentToolPaths(ctx, invalid); !errors.Is(err, workspace.ErrInvalidAssignmentID) {
			t.Fatalf("collection without valid Participant identity = %v", err)
		}
		for ref := range wantContents {
			assertRetained(ref)
		}
	}
	if err := restarted.CleanupAssignmentToolPaths(ctx, assignmentID); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []execution.CaptureRef{publication, terminal} {
		reader, err := restarted.OpenCapture(ctx, ref)
		if reader != nil {
			_ = reader.Close()
		}
		if err == nil {
			t.Error("collected Participant capture remains accessible")
		}
		if _, err := restarted.Capture(ctx, ref.Request); !errors.Is(err, execution.ErrUnsupportedRequirement) {
			t.Errorf("collected operation still has retained storage: %v", err)
		}
	}
	assertRetained(other)
	if ref, err := restarted.TerminalCapture(ctx, otherRequest.Workspace.Key); err != nil || ref != other {
		t.Fatalf("uncollected Participant terminal evidence = %#v, %v", ref, err)
	}
}
