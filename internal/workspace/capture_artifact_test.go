package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type captureTarEntry struct {
	name string
	kind byte
	mode int64
	link string
	body string
}

// The synthetic inventory has valid HEAD/ref identities but dummy index/object
// contents, so artifact checks do not depend on successful Git history queries.
func captureInventory() []captureTarEntry {
	return []captureTarEntry{
		{name: ".git", kind: tar.TypeDir, mode: 0o755},
		{name: ".git/HEAD", kind: tar.TypeReg, mode: 0o644, body: "ref: refs/heads/main\n"},
		{name: ".git/index", kind: tar.TypeReg, mode: 0o644, body: "index bytes"},
		{name: ".git/objects", kind: tar.TypeDir, mode: 0o755},
		{name: ".git/refs", kind: tar.TypeDir, mode: 0o755},
		{name: ".git/refs/heads", kind: tar.TypeDir, mode: 0o755},
		{name: ".git/refs/heads/main", kind: tar.TypeReg, mode: 0o644, body: strings.Repeat("a", 40) + "\n"},
	}
}

func encodeCaptureTar(t *testing.T, rooted bool, entries []captureTarEntry) []byte {
	t.Helper()
	var content bytes.Buffer
	writer := tar.NewWriter(&content)
	if rooted {
		if err := writer.WriteHeader(&tar.Header{Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		name := entry.name
		if rooted {
			name = "workspace"
			if entry.name != "" {
				name += "/" + entry.name
			}
		}
		header := &tar.Header{Name: name, Typeflag: entry.kind, Mode: entry.mode, Linkname: entry.link,
			Size: int64(len(entry.body)), Uid: 1234, Gid: 5678, Uname: "backend-user", Gname: "backend-group",
			ModTime: time.Unix(1234567, 0), Format: tar.FormatPAX}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return content.Bytes()
}

func artifactRef(content []byte) execution.CaptureRef {
	digest := sha256.Sum256(content)
	return execution.CaptureRef{SHA256: hex.EncodeToString(digest[:])}
}

func TestCaptureArtifactRetainsContentsExecutableBitsAndSymlinkTargets(t *testing.T) {
	outside := t.TempDir()
	marker := filepath.Join(outside, "marker")
	if err := os.WriteFile(marker, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := append(captureInventory(),
		captureTarEntry{name: "bin", kind: tar.TypeDir, mode: 0o755},
		captureTarEntry{name: "bin/tool", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
		captureTarEntry{name: "read-only", kind: tar.TypeReg, mode: 0o444, body: "immutable content"},
		captureTarEntry{name: "tool", kind: tar.TypeSymlink, mode: 0o777, link: "bin/tool"},
		captureTarEntry{name: "external", kind: tar.TypeSymlink, mode: 0o777, link: marker},
	)
	var normalized bytes.Buffer
	digest, err := normalizeCapture(context.Background(), bytes.NewReader(encodeCaptureTar(t, true, entries)), &normalized)
	if err != nil {
		t.Fatal(err)
	}
	ref := artifactRef(normalized.Bytes())
	if digest != ref.SHA256 {
		t.Fatalf("returned digest %s does not identify transferred bytes %s", digest, ref.SHA256)
	}
	reader := tar.NewReader(bytes.NewReader(normalized.Bytes()))
	got := make(map[string]captureTarEntry)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		got[header.Name] = captureTarEntry{name: header.Name, kind: header.Typeflag, mode: header.Mode, link: header.Linkname, body: string(body)}
	}
	for _, want := range entries {
		if got[want.name] != want {
			t.Errorf("portable entry %s = %#v, want %#v", want.name, got[want.name], want)
		}
	}
	if len(got) != len(entries) {
		t.Errorf("portable inventory has %d entries, want %d", len(got), len(entries))
	}
	destination := t.TempDir()
	if err := materializeCapture(context.Background(), bytes.NewReader(normalized.Bytes()), destination, ref); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "bin", "tool"))
	if err != nil || info.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("materialized executable = %v, %v", info, err)
	}
	if body, err := os.ReadFile(filepath.Join(destination, "read-only")); err != nil || string(body) != "immutable content" {
		t.Fatalf("materialized contents = %q, %v", body, err)
	}
	for _, name := range []string{"tool", "external"} {
		if target, err := os.Readlink(filepath.Join(destination, name)); err != nil || target != got[name].link {
			t.Errorf("materialized symlink %s = %q, %v", name, target, err)
		}
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "untouched" {
		t.Fatalf("external symlink target changed: %q, %v", body, err)
	}
}

func TestCaptureArtifactRestoresOnlyTrustedGitConfigForCapturedObjectFormat(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, headKind := range []string{"detached", "loose ref", "packed ref"} {
			t.Run(format+"/"+headKind, func(t *testing.T) {
				length := 40
				if format == "sha256" {
					length = 64
				}
				revision := strings.Repeat("a", length)
				var entries []captureTarEntry
				for _, entry := range captureInventory() {
					if entry.name == ".git/HEAD" && headKind == "detached" {
						entry.body = revision + "\n"
					}
					if entry.name == ".git/refs/heads/main" {
						if headKind == "packed ref" {
							continue
						}
						entry.body = revision + "\n"
					}
					entries = append(entries, entry)
				}
				if headKind == "packed ref" {
					entries = append(entries, captureTarEntry{name: ".git/packed-refs", kind: tar.TypeReg, mode: 0o644,
						body: "# pack-refs with: peeled fully-peeled sorted\n" + revision + " refs/heads/main\n"})
				}
				// Conflicting source format and executable settings must never
				// determine the trusted inspector's repository configuration.
				entries = append(entries,
					captureTarEntry{name: ".git/config", kind: tar.TypeReg, mode: 0o644,
						body: "[core]\nrepositoryformatversion = 1\nworktree = /untrusted\nfsmonitor = /untrusted\n[extensions]\nobjectformat = untrusted\n[include]\npath = /untrusted\n[alias]\ninspect = !untrusted\n[uploadpack]\npackObjectsHook = /untrusted\n"},
					captureTarEntry{name: ".git/config.worktree", kind: tar.TypeReg, mode: 0o644, body: "[include]\npath = /untrusted\n"},
					captureTarEntry{name: ".git/hooks", kind: tar.TypeDir, mode: 0o755},
					captureTarEntry{name: ".git/hooks/post-checkout", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\nexit 77\n"},
				)
				content := encodeCaptureTar(t, false, entries)
				destination := t.TempDir()
				if err := materializeCapture(context.Background(), bytes.NewReader(content), destination, artifactRef(content)); err != nil {
					t.Fatal(err)
				}
				config, err := os.ReadFile(filepath.Join(destination, ".git", "config"))
				if err != nil {
					t.Fatal(err)
				}
				for _, sourceSetting := range []string{"untrusted", "include", "alias", "uploadpack", "worktree", "fsmonitor"} {
					if strings.Contains(strings.ToLower(string(config)), sourceSetting) {
						t.Errorf("trusted config retained source-controlled setting %q: %s", sourceSetting, config)
					}
				}
				for _, discarded := range []string{"config.worktree", "hooks"} {
					if _, err := os.Lstat(filepath.Join(destination, ".git", discarded)); !os.IsNotExist(err) {
						t.Errorf("unsafe metadata %s survived materialization: %v", discarded, err)
					}
				}
				command := exec.Command("git", "rev-parse", "--show-object-format")
				command.Dir = destination
				command.Env = commandEnvironment("", "")
				output, err := command.CombinedOutput()
				if err != nil || strings.TrimSpace(string(output)) != format {
					t.Fatalf("Git interpreted captured %s HEAD as %q, %v, want %s", headKind, output, err, format)
				}
			})
		}
	}
}

func TestCaptureArtifactRejectsUnsafeAndIncompleteInventory(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []captureTarEntry
	}{
		{name: "parent traversal", entries: []captureTarEntry{{name: "../escape", kind: tar.TypeReg, body: "escape"}}},
		{name: "absolute path", entries: []captureTarEntry{{name: "/escape", kind: tar.TypeReg, body: "escape"}}},
		{name: "unclean path", entries: []captureTarEntry{{name: "a/../escape", kind: tar.TypeReg, body: "escape"}}},
		{name: "backslash path", entries: []captureTarEntry{{name: "a\\escape", kind: tar.TypeReg}}},
		{name: "hardlink", entries: []captureTarEntry{{name: "linked", kind: tar.TypeLink, link: ".git/HEAD"}}},
		{name: "fifo", entries: []captureTarEntry{{name: "pipe", kind: tar.TypeFifo}}},
		{name: "device", entries: []captureTarEntry{{name: "device", kind: tar.TypeChar}}},
		{name: "symlink ancestor", entries: []captureTarEntry{{name: "link", kind: tar.TypeSymlink, link: "../outside"}, {name: "link/escape", kind: tar.TypeReg, body: "escape"}}},
		{name: "file ancestor", entries: []captureTarEntry{{name: "file", kind: tar.TypeReg}, {name: "file/child", kind: tar.TypeReg}}},
		{name: "missing ancestor", entries: []captureTarEntry{{name: "missing/child", kind: tar.TypeReg}}},
		{name: "git metadata symlink", entries: []captureTarEntry{{name: ".git/config", kind: tar.TypeSymlink, link: "../../outside"}}},
		{name: "duplicate entry", entries: []captureTarEntry{{name: "duplicate", kind: tar.TypeReg}, {name: "duplicate", kind: tar.TypeReg}}},
		{name: "overlong path", entries: []captureTarEntry{{name: strings.Repeat("x", 4097), kind: tar.TypeReg}}},
		{name: "overlong symlink", entries: []captureTarEntry{{name: "link", kind: tar.TypeSymlink, link: strings.Repeat("x", 4097)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := append(captureInventory(), test.entries...)
			if _, err := normalizeCapture(context.Background(), bytes.NewReader(encodeCaptureTar(t, true, entries)), io.Discard); !errors.Is(err, ErrInvalidCapture) {
				t.Errorf("normalize unsafe inventory = %v", err)
			}
			content := encodeCaptureTar(t, false, entries)
			parent := t.TempDir()
			destination := filepath.Join(parent, "stage")
			if err := os.Mkdir(destination, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := materializeCapture(context.Background(), bytes.NewReader(content), destination, artifactRef(content)); !errors.Is(err, ErrInvalidCapture) {
				t.Errorf("materialize unsafe inventory = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Errorf("capture wrote outside staging: %v", err)
			}
		})
	}
	for _, missing := range []string{".git", ".git/HEAD", ".git/index", ".git/objects"} {
		t.Run("missing "+missing, func(t *testing.T) {
			var entries []captureTarEntry
			for _, entry := range captureInventory() {
				if entry.name != missing {
					entries = append(entries, entry)
				}
			}
			if _, err := normalizeCapture(context.Background(), bytes.NewReader(encodeCaptureTar(t, true, entries)), io.Discard); !errors.Is(err, ErrInvalidCapture) {
				t.Errorf("normalize missing inventory = %v", err)
			}
			content := encodeCaptureTar(t, false, entries)
			if err := materializeCapture(context.Background(), bytes.NewReader(content), t.TempDir(), artifactRef(content)); !errors.Is(err, ErrInvalidCapture) {
				t.Errorf("materialize missing inventory = %v", err)
			}
		})
	}
}

func TestCaptureArtifactRejectsMalformedRootAndArchive(t *testing.T) {
	valid := encodeCaptureTar(t, true, captureInventory())
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "no root", content: encodeCaptureTar(t, false, captureInventory())},
		{name: "truncated header", content: valid[:256]},
		{name: "truncated contents", content: valid[:1025]},
		{name: "bytes after end marker", content: append(bytes.Clone(valid), []byte("unaccounted inventory")...)},
		{name: "second archive", content: append(bytes.Clone(valid), valid...)},
		{name: "duplicate root directory", content: encodeCaptureTar(t, true, append(captureInventory(), captureTarEntry{name: "", kind: tar.TypeDir, mode: 0o755}))},
		{name: "root replaced with regular file", content: encodeCaptureTar(t, true, append(captureInventory(), captureTarEntry{name: "", kind: tar.TypeReg, body: "replacement"}))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeCapture(context.Background(), bytes.NewReader(test.content), io.Discard); !errors.Is(err, ErrInvalidCapture) {
				t.Fatalf("malformed capture accepted: %v", err)
			}
		})
	}
	portable := encodeCaptureTar(t, false, captureInventory())
	for _, suffix := range [][]byte{[]byte("unaccounted inventory"), portable} {
		content := append(bytes.Clone(portable), suffix...)
		if err := materializeCapture(context.Background(), bytes.NewReader(content), t.TempDir(), artifactRef(content)); !errors.Is(err, ErrInvalidCapture) {
			t.Errorf("materializer accepted bytes beyond archive end: %v", err)
		}
	}
}

// Generate headers lazily so the entry-limit regression does not itself retain
// a large archive or create 100,000 filesystem objects.
type captureEntryStream struct {
	pending []byte
	next    int
	limit   int
}

func (stream *captureEntryStream) Read(buffer []byte) (int, error) {
	if len(stream.pending) == 0 {
		if stream.next >= stream.limit {
			return 0, io.EOF
		}
		var encoded bytes.Buffer
		writer := tar.NewWriter(&encoded)
		if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("workspace/file-%06d", stream.next), Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
			return 0, err
		}
		stream.pending = encoded.Bytes()
		stream.next++
	}
	n := copy(buffer, stream.pending)
	stream.pending = stream.pending[n:]
	return n, nil
}

func TestCaptureArtifactEnforcesEntryBounds(t *testing.T) {
	// The entry budget applies to all inventory, including ignored/empty files.
	prefix := encodeCaptureTar(t, true, captureInventory())
	prefix = prefix[:len(prefix)-1024]
	stream := &captureEntryStream{limit: maxCaptureEntries + 1}
	if _, err := normalizeCapture(context.Background(), io.MultiReader(bytes.NewReader(prefix), stream), io.Discard); !errors.Is(err, ErrInvalidCapture) {
		t.Fatalf("entry budget = %v", err)
	}
	if stream.next > maxCaptureEntries+1 {
		t.Fatalf("entry budget consumed unbounded inventory: %d", stream.next)
	}
}

type captureCountingReader struct {
	reader io.Reader
	read   int
}

func (reader *captureCountingReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	reader.read += n
	return n, err
}

func TestCaptureArtifactRejectsOversizedHeaderWithoutReadingPayload(t *testing.T) {
	prefix := encodeCaptureTar(t, true, captureInventory())
	prefix = prefix[:len(prefix)-1024]
	var header bytes.Buffer
	writer := tar.NewWriter(&header)
	if err := writer.WriteHeader(&tar.Header{Name: "workspace/huge", Typeflag: tar.TypeReg, Size: maxCaptureBytes + 1}); err != nil {
		t.Fatal(err)
	}
	payload := &captureCountingReader{reader: strings.NewReader("must not read payload")}
	if _, err := normalizeCapture(context.Background(), io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(header.Bytes()), payload), io.Discard); !errors.Is(err, ErrInvalidCapture) {
		t.Fatalf("oversized header = %v", err)
	}
	if payload.read != 0 {
		t.Fatalf("oversized declaration consumed %d payload bytes", payload.read)
	}
}

func TestCaptureArtifactDigestFailureAndCancellationDoNotMaterialize(t *testing.T) {
	content := encodeCaptureTar(t, false, captureInventory())
	for _, name := range []string{"wrong digest", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ref := artifactRef(content)
			want := ErrInvalidCapture
			if name == "wrong digest" {
				ref.SHA256 = strings.Repeat("0", 64)
			} else {
				cancel()
				want = context.Canceled
			}
			destination := t.TempDir()
			if err := materializeCapture(ctx, bytes.NewReader(content), destination, ref); !errors.Is(err, want) {
				t.Fatalf("materialize = %v, want %v", err, want)
			}
			entries, err := os.ReadDir(destination)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid capture wrote entries: %v, %v", entries, err)
			}
		})
	}
	if err := VerifyCapture(bytes.NewReader(append(bytes.Clone(content), 1)), artifactRef(content)); !errors.Is(err, ErrInvalidCapture) {
		t.Fatalf("digest failed to cover trailing bytes: %v", err)
	}
}

type storedCaptureSource struct {
	content []byte
	calls   int
}

func (source *storedCaptureSource) ReadWorkspace(_ context.Context, _ execution.CaptureRequest, consume func(io.Reader) error) error {
	source.calls++
	return consume(bytes.NewReader(source.content))
}

func newStoredCapture(t *testing.T) (*Lifecycle, *storedCaptureSource, execution.CaptureRequest) {
	t.Helper()
	root := t.TempDir()
	source := &storedCaptureSource{content: encodeCaptureTar(t, true, captureInventory())}
	lifecycle, err := New(Options{
		WorkspaceRoot: filepath.Join(root, "workspaces"), PublicationRoot: filepath.Join(root, "publications"),
		MiseRoot: filepath.Join(root, "mise"), CaptureRoot: filepath.Join(root, "captures"), CaptureSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := execution.CaptureRequest{Workspace: execution.WorkspaceRef{Key: execution.Key{
		AssignmentID: "12345678-1234-4234-8234-123456789abc", AgentSessionID: "12345678-1234-4234-8234-123456789abd",
		AgentTurnID: "12345678-1234-4234-8234-123456789abe", ExecutionEpoch: 7,
		RuntimeProfileName: "pi", RuntimeProfileVersion: "v1",
	}}, OperationID: "operation", Purpose: execution.PublicationCapture}
	return lifecycle, source, request
}

func TestCaptureStorageRejectsCorruptOrMissingEvidence(t *testing.T) {
	for _, name := range []string{"missing archive", "changed archive", "missing manifest", "invalid manifest", "wrong version", "wrong owner", "wrong digest"} {
		t.Run(name, func(t *testing.T) {
			lifecycle, source, request := newStoredCapture(t)
			ref, err := lifecycle.Capture(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			directory, err := lifecycle.capturePath(request)
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(directory, "manifest.json")
			switch name {
			case "missing archive":
				err = os.Remove(filepath.Join(directory, "workspace.tar"))
			case "changed archive":
				err = os.WriteFile(filepath.Join(directory, "workspace.tar"), []byte("corrupt archive"), 0o600)
			case "missing manifest":
				err = os.Remove(manifestPath)
			case "invalid manifest":
				err = os.WriteFile(manifestPath, []byte("{broken"), 0o600)
			default:
				manifest := captureManifest{Version: captureVersion, Ref: ref}
				if name == "wrong version" {
					manifest.Version++
				} else if name == "wrong owner" {
					manifest.Ref.Request.Workspace.Key.AgentSessionID = "12345678-1234-4234-8234-123456789abf"
				} else {
					manifest.Ref.SHA256 = strings.Repeat("z", 64)
				}
				content, marshalErr := json.Marshal(manifest)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(manifestPath, content, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			reader, err := lifecycle.OpenCapture(context.Background(), ref)
			if reader != nil {
				_ = reader.Close()
			}
			if err == nil {
				t.Error("OpenCapture accepted unavailable/corrupt evidence")
			}
			// A retry must report missing protected evidence, never replace it
			// with an observation of a later source or return an unusable ref.
			if _, err := lifecycle.Capture(context.Background(), request); err == nil {
				t.Error("Capture retry accepted unavailable/corrupt sealed evidence")
			}
			if source.calls != 1 {
				t.Errorf("corruption caused recapture: source calls = %d", source.calls)
			}
		})
	}
}

func TestCaptureStorageRejectsDeletedWholeOperationAfterRestart(t *testing.T) {
	ctx := context.Background()
	lifecycle, source, request := newStoredCapture(t)
	ref, err := lifecycle.Capture(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := filepath.Glob(filepath.Join(lifecycle.captureRoot, "*", "*", "*", "workspace.tar"))
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("discover sealed artifact = %v, %v", artifacts, err)
	}
	// Discover the independent seal inventory rather than deriving the ledger's
	// key or copying its implementation's path construction.
	seals := make(map[string][]byte)
	err = filepath.WalkDir(lifecycle.captureRoot, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		for _, component := range strings.Split(filepath.ToSlash(name), "/") {
			if component == ".seals" {
				content, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				seals[name] = content
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seals) == 0 {
		t.Error("successful capture has no independent durable seal inventory")
	}
	if err := os.RemoveAll(filepath.Dir(artifacts[0])); err != nil {
		t.Fatal(err)
	}
	for name, want := range seals {
		if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, want) {
			t.Errorf("removing operation directory removed or changed independent seal: %v", err)
		}
	}
	source.content = encodeCaptureTar(t, true, append(captureInventory(), captureTarEntry{name: "later-source", kind: tar.TypeReg, body: "must not replace lost evidence"}))
	restarted, err := New(Options{
		WorkspaceRoot: lifecycle.workspaceRoot, PublicationRoot: lifecycle.publicationRoot, MiseRoot: lifecycle.miseRoot,
		CaptureRoot: lifecycle.captureRoot, CaptureSource: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := restarted.OpenCapture(ctx, ref)
	if reader != nil {
		_ = reader.Close()
	}
	if err == nil {
		t.Error("deleted artifact remained accessible")
	}
	if _, err := restarted.Capture(ctx, request); !errors.Is(err, ErrInvalidCapture) {
		t.Errorf("retry after whole-operation deletion = %v, want lost-evidence failure", err)
	}
	if source.calls != 1 {
		t.Errorf("deleted sealed operation re-observed later source %d times", source.calls-1)
	}
}

type blockedCaptureSource struct {
	content []byte
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (source *blockedCaptureSource) ReadWorkspace(ctx context.Context, _ execution.CaptureRequest, consume func(io.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source.calls.Add(1)
	source.once.Do(func() { close(source.entered) })
	select {
	case <-source.release:
		return consume(bytes.NewReader(source.content))
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCaptureStorageCancelledLockWaiterDoesNotWaitForActiveSource(t *testing.T) {
	lifecycle, _, request := newStoredCapture(t)
	source := &blockedCaptureSource{
		content: encodeCaptureTar(t, true, captureInventory()), entered: make(chan struct{}), release: make(chan struct{}),
	}
	lifecycle.captureSource = source
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 10*time.Second)
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(source.release) })
		cancelFirst()
		cancelSecond()
		workers.Wait()
	})
	type outcome struct {
		ref execution.CaptureRef
		err error
	}
	first := make(chan outcome, 1)
	workers.Go(func() {
		ref, err := lifecycle.Capture(firstCtx, request)
		first <- outcome{ref: ref, err: err}
	})
	select {
	case <-source.entered:
	case result := <-first:
		t.Fatalf("first capture never held source: %v", result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("first capture never entered source")
	}
	second := make(chan error, 1)
	secondStarted := make(chan struct{})
	workers.Go(func() {
		close(secondStarted)
		_, err := lifecycle.Capture(secondCtx, request)
		second <- err
	})
	<-secondStarted
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Establish that the second call is actually waiting on the held lock
		// before cancellation; an entry-only context check would not suffice.
		lifecycle.locksMu.Lock()
		queued := false
		for _, entry := range lifecycle.publicationLocks {
			queued = queued || entry.refs >= 2
		}
		lifecycle.locksMu.Unlock()
		if queued {
			break
		}
		select {
		case err := <-second:
			t.Fatalf("second capture returned before waiting on active source: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("second capture did not queue behind the active operation")
		}
		runtime.Gosched()
	}
	cancelSecond()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled capture waiter = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled capture waiter remained blocked behind active source")
	}
	select {
	case result := <-first:
		t.Fatalf("first capture ended before source release: %v", result.err)
	default:
	}
	release.Do(func() { close(source.release) })
	select {
	case result := <-first:
		if result.err != nil || result.ref.Request != request {
			t.Fatalf("active capture after orderly release = %#v, %v", result.ref, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first capture did not complete after source release")
	}
	if source.calls.Load() != 1 {
		t.Fatalf("cancelled waiter entered source: calls = %d", source.calls.Load())
	}
}

func TestCaptureStorageEnforcesImmutableProvenance(t *testing.T) {
	lifecycle, source, request := newStoredCapture(t)
	ref, err := lifecycle.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session", "profile", "seed", "digest", "epoch", "operation", "purpose"} {
		t.Run(name, func(t *testing.T) {
			altered := ref
			switch name {
			case "session":
				altered.Request.Workspace.Key.AgentSessionID = "12345678-1234-4234-8234-123456789abf"
			case "profile":
				altered.Request.Workspace.Key.RuntimeProfileVersion = "v2"
			case "seed":
				altered.Request.Workspace.Seed = strings.Repeat("a", 40)
			case "digest":
				altered.SHA256 = strings.Repeat("a", 64)
			case "epoch":
				altered.Request.Workspace.Key.ExecutionEpoch++
			case "operation":
				altered.Request.OperationID = "another-operation"
			case "purpose":
				altered.Request.Purpose = execution.TerminalCapture
			}
			reader, err := lifecycle.OpenCapture(context.Background(), altered)
			if reader != nil {
				_ = reader.Close()
			}
			if !errors.Is(err, ErrInvalidCapture) {
				t.Fatalf("altered %s capture = %v", name, err)
			}
			if name == "session" || name == "profile" || name == "seed" {
				if _, err := lifecycle.Capture(context.Background(), altered.Request); !errors.Is(err, ErrInvalidCapture) {
					t.Fatalf("retry changed immutable %s provenance: %v", name, err)
				}
			}
		})
	}
	if source.calls != 1 {
		t.Fatal("reading invalid provenance re-observed workspace")
	}
}

func TestCaptureStorageSeparatesPurposeOperationAndExecutionEpoch(t *testing.T) {
	lifecycle, source, original := newStoredCapture(t)
	publication, err := lifecycle.Capture(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"purpose", "operation", "epoch"} {
		t.Run(name, func(t *testing.T) {
			request := original
			switch name {
			case "purpose":
				request.Purpose = execution.TerminalCapture
			case "operation":
				request.OperationID = "new-operation"
			case "epoch":
				request.Workspace.Key.ExecutionEpoch++
			}
			source.content = encodeCaptureTar(t, true, append(captureInventory(), captureTarEntry{name: "later", kind: tar.TypeReg, body: name}))
			ref, err := lifecycle.Capture(context.Background(), request)
			if err != nil || ref.Request != request || ref.SHA256 == publication.SHA256 {
				t.Fatalf("distinct %s capture = %#v, %v", name, ref, err)
			}
			reader, err := lifecycle.OpenCapture(context.Background(), publication)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyCapture(reader, publication); err != nil {
				t.Error(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if source.calls != 4 {
		t.Fatalf("distinct operations did not get fresh observations: %d", source.calls)
	}
}

func TestCaptureStorageRejectsInvalidRequestsAndUnsupportedSource(t *testing.T) {
	for _, name := range []string{"assignment", "session", "turn", "epoch", "profile name", "profile version", "operation", "operation nul", "operation length", "purpose"} {
		t.Run(name, func(t *testing.T) {
			lifecycle, source, request := newStoredCapture(t)
			switch name {
			case "assignment":
				request.Workspace.Key.AssignmentID = "../escape"
			case "session":
				request.Workspace.Key.AgentSessionID = ""
			case "turn":
				request.Workspace.Key.AgentTurnID = "not-a-uuid"
			case "epoch":
				request.Workspace.Key.ExecutionEpoch = 0
			case "profile name":
				request.Workspace.Key.RuntimeProfileName = ""
			case "profile version":
				request.Workspace.Key.RuntimeProfileVersion = ""
			case "operation":
				request.OperationID = ""
			case "operation nul":
				request.OperationID = "operation\x00suffix"
			case "operation length":
				request.OperationID = strings.Repeat("x", 257)
			case "purpose":
				request.Purpose = "unknown"
			}
			if _, err := lifecycle.Capture(context.Background(), request); !errors.Is(err, ErrInvalidCapture) {
				t.Fatalf("invalid capture request = %v", err)
			}
			if source.calls != 0 {
				t.Fatal("invalid identity reached workspace source")
			}
		})
	}
	lifecycle, _, request := newStoredCapture(t)
	lifecycle.captureSource = nil
	if _, err := lifecycle.Capture(context.Background(), request); !errors.Is(err, execution.ErrUnsupportedRequirement) {
		t.Fatalf("capture without consistency provider = %v", err)
	}
	if _, err := lifecycle.TerminalCapture(context.Background(), request.Workspace.Key); err == nil {
		t.Fatal("missing terminal capture treated as available evidence")
	}
}

func TestCaptureStorageConcurrentSameOperationSealsOneObservation(t *testing.T) {
	lifecycle, source, request := newStoredCapture(t)
	const callers = 8
	refs := make(chan execution.CaptureRef, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Go(func() {
			ref, err := lifecycle.Capture(context.Background(), request)
			refs <- ref
			errors <- err
		})
	}
	group.Wait()
	close(refs)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var want execution.CaptureRef
	for ref := range refs {
		if want == (execution.CaptureRef{}) {
			want = ref
		} else if ref != want {
			t.Fatalf("same operation produced different sealed evidence: %#v vs %#v", ref, want)
		}
	}
	if source.calls != 1 {
		t.Fatalf("concurrent operation observed source %d times", source.calls)
	}
}
