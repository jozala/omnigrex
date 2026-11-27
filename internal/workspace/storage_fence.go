package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

type storageFenceKey struct{}
type storageFenceScope struct {
	lifecycle *Lifecycle
	id        string
	file      *os.File
	active    atomic.Bool
}

// WithParticipantStorageWriter protects host preparation, state initialization,
// publication, captures and tool paths. The durable admission callback runs only
// after the physical lock, so a late writer cannot enter a COLLECTING identity.
func (lifecycle *Lifecycle) WithParticipantStorageWriter(ctx context.Context, id string, operation func(context.Context) error) error {
	return lifecycle.withStorageWriter(ctx, id, operation)
}
func (lifecycle *Lifecycle) withStorageWriter(ctx context.Context, id string, operation func(context.Context) error) error {
	if lifecycle == nil {
		return ErrInvalidOptions
	}
	if !uuidtext.Valid(id) {
		return ErrInvalidAssignmentID
	}
	if operation == nil {
		return ErrUnsafeAssignmentPath
	}
	if scope, ok := ctx.Value(storageFenceKey{}).(*storageFenceScope); ok && scope.lifecycle == lifecycle && scope.id == id && scope.active.Load() {
		return operation(ctx)
	}
	// Isolated library/test callers supply their own authority. Production always
	// configures durable admission and therefore the cross-process physical lock.
	if lifecycle.storageWriter == nil {
		return operation(ctx)
	}
	return lifecycle.withStorageLock(ctx, id, false, func(ctx context.Context, file *os.File) error {
		scope := &storageFenceScope{lifecycle: lifecycle, id: id, file: file}
		scope.active.Store(true)
		defer scope.active.Store(false)
		return lifecycle.storageWriter(context.WithValue(ctx, storageFenceKey{}, scope), id, operation)
	})
}
func (lifecycle *Lifecycle) WithParticipantStorageCollection(ctx context.Context, id string, operation func(context.Context) error) error {
	if lifecycle == nil || !uuidtext.Valid(id) || operation == nil {
		return ErrUnsafeAssignmentPath
	}
	if lifecycle.storageWriter == nil {
		return operation(ctx)
	}
	return lifecycle.withStorageLock(ctx, id, true, func(ctx context.Context, _ *os.File) error { return operation(ctx) })
}

func (lifecycle *Lifecycle) withStorageLock(ctx context.Context, id string, exclusive bool, operation func(context.Context, *os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := ensureStorageFenceRoot(lifecycle.workspaceRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	const locks = ".participant-storage-locks"
	if err := unix.Mkdirat(int(root.Fd()), locks, 0700); err != nil && err != unix.EEXIST {
		return err
	}
	directory, err := openStorageChild(root, locks)
	if err != nil {
		return err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), id, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(lifecycle.workspaceRoot, locks, id))
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return ErrUnsafeAssignmentPath
	}
	if err := storageLeafSameMount(directory, id); err != nil {
		return err
	}
	kind := unix.LOCK_SH
	if exclusive {
		kind = unix.LOCK_EX
	}
	for {
		err := unix.Flock(fd, kind|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	// Never unlink a lock inode: removing it could create two independent owners.
	// Git subprocesses inherit the open lock description through ExtraFiles, so
	// orchestrator death does not release exclusion while a child is still writing.
	return operation(ctx, file)
}

// Create missing configured components relative to pinned no-follow parents.
func ensureStorageFenceRoot(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrUnsafeAssignmentPath
	}
	parent, _, err := openDirectoryNoFollow("/")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name == "" {
			continue
		}
		child, err := openDirectoryAtNoFollow(int(parent.Fd()), name)
		if os.IsNotExist(err) {
			if err = unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil && err != unix.EEXIST {
				parent.Close()
				return nil, err
			}
			child, err = openDirectoryAtNoFollow(int(parent.Fd()), name)
		}
		parent.Close()
		if err != nil {
			return nil, err
		}
		parent = child
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &stat); err != nil {
		parent.Close()
		return nil, err
	}
	if int(stat.Uid) != os.Geteuid() {
		parent.Close()
		return nil, ErrUnsafeAssignmentPath
	}
	return parent, nil
}
