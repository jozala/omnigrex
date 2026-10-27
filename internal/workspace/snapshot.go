package workspace

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/gitdelivery"
)

var ErrSnapshotUnavailable = errors.New("default-branch snapshot acquisition unavailable")

// AcquireSnapshot uses a fresh controller-owned bare repository, never an
// agent-modified checkout/config. No credentials or remote configuration are
// delivered. The pack contains all ancestry, not a shallow verification view.
func (lifecycle *Lifecycle) AcquireSnapshot(ctx context.Context, remote, credential, sha, ref string) (gitdelivery.Snapshot, error) {
	if validateRemote(remote) != nil || validateCredential(credential) != nil || len(sha) != 40 {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	if normalized, err := normalizeObjectID(sha); err != nil || normalized != sha {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// PublicationRoot is controller-owned and never mounted into the Runtime Process.
	if err := os.MkdirAll(lifecycle.publicationRoot, 0700); err != nil {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	dir, err := os.MkdirTemp(lifecycle.publicationRoot, ".default-snapshot-")
	if err != nil {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	defer os.RemoveAll(dir)
	run := func(credential string, args ...string) error {
		// Bound individual pack/output files even if a server ignores negotiation.
		// The shell and arguments are controller-owned; diagnostics are discarded.
		argv := append([]string{"-c", `ulimit -f 262144; exec "$@"`, "snapshot-git", lifecycle.gitExecutable}, args...)
		command := exec.CommandContext(ctx, "/bin/sh", argv...)
		command.Dir = dir
		command.Env = commandEnvironment(credential, "")
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		command.WaitDelay = 2 * time.Second
		return command.Run()
	}
	if run("", "init", "--bare", "--object-format=sha1", ".") != nil || run(credential, "-c", "fetch.fsckObjects=true", "-c", "fetch.unpackLimit=0", "fetch", "--no-tags", "--no-auto-maintenance", "--no-write-fetch-head", "--", remote, sha) != nil || run("", "cat-file", "-e", sha+"^{commit}") != nil || run("", "fsck", "--strict", "--full", sha) != nil || run("", "update-ref", "refs/heads/snapshot", sha) != nil || run("", "repack", "-ad") != nil || run("", "fsck", "--strict", "--full", sha) != nil {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) != 1 {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	packName := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(packs[0]), "pack-"), ".pack")
	read := func(name string) ([]byte, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, gitdelivery.MaxPackBytes+1))
		if len(b) > gitdelivery.MaxPackBytes {
			return nil, ErrSnapshotUnavailable
		}
		return b, err
	}
	pack, err := read(packs[0])
	if err != nil {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	index, err := read(strings.TrimSuffix(packs[0], ".pack") + ".idx")
	if err != nil {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	snapshot := gitdelivery.Snapshot{SHA: sha, Ref: ref, PackName: packName, Pack: pack, Index: index}
	if _, err := gitdelivery.Files(snapshot); err != nil {
		return gitdelivery.Snapshot{}, ErrSnapshotUnavailable
	}
	return snapshot, nil
}
