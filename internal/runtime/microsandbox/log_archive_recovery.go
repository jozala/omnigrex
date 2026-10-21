package microsandbox

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"golang.org/x/sys/unix"
)

// flock is tied to this resource directory's inode, across controller processes.
// No lock file is unlinked/recreated, and a crash releases the lock automatically.
func lockLogArchive(ctx context.Context, directory *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(directory.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
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
}

func logArchiveStageName(name string) bool {
	suffix, ok := strings.CutPrefix(name, ".archive-")
	if !ok || len(suffix) < 26 {
		return false
	}
	for _, character := range suffix {
		if !(character >= 'A' && character <= 'Z' || character >= '2' && character <= '7') {
			return false
		}
	}
	return true
}

// A name lookup alone cannot bind filesystem bytes to a native resource. Open
// the directory first, revalidate exact identity, then confirm that the name
// still denotes this inode. Subsequent reads stay on the pinned directory FD,
// even if native removal/recreation replaces the name after this check.
func (backend *Backend) openRuntimeLogSource(ctx context.Context, resource metadata) (*os.File, error) {
	location := filepath.Join(backend.options.Home, "sandboxes", resource.Name, "logs")
	source, err := openLogDirectory(location, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	verified, err := backend.resolveRef(ctx, resourceRef(resource))
	if err == nil && (!maps.Equal(verified.Labels, resource.Labels) || !terminalEnvironmentStatus(verified.Status)) {
		err = execution.ErrConflict
	}
	if err != nil {
		source.Close()
		return nil, err
	}
	current, err := openLogDirectory(location, false)
	if err != nil {
		source.Close()
		return nil, errors.Join(execution.ErrConflict, err)
	}
	defer current.Close()
	openedInfo, openedErr := source.Stat()
	currentInfo, currentErr := current.Stat()
	if openedErr != nil || currentErr != nil || !os.SameFile(openedInfo, currentInfo) {
		source.Close()
		return nil, errors.Join(execution.ErrConflict, openedErr, currentErr)
	}
	return source, nil
}
