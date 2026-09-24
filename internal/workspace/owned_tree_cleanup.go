package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Go's module cache contains read-only directories. Restore only the owner's
// directory permissions, using no-follow descriptors so an agent-created link
// cannot redirect chmod outside its assignment data.
func makeOwnedTreeDirectoriesWritable(ctx context.Context, path string) error {
	parent, _, err := openDirectoryNoFollow(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	var stat unix.Stat_t
	name := filepath.Base(path)
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	directory, err := openOwnedDirectoryAtNoFollow(int(parent.Fd()), name, stat)
	if err != nil {
		return err
	}
	defer directory.Close()
	return makeOwnedDirectoriesWritable(ctx, directory)
}

func openOwnedDirectoryAtNoFollow(parentFD int, name string, stat unix.Stat_t) (*os.File, error) {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("%w: assignment directory is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	child, err := openDirectoryAtNoFollow(parentFD, name)
	if errors.Is(err, unix.EACCES) && stat.Mode&0o500 != 0o500 {
		if err := restoreUnreadableOwnedDirectory(parentFD, name, stat); err != nil {
			return nil, err
		}
		child, err = openDirectoryAtNoFollow(parentFD, name)
	}
	if err != nil {
		return nil, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(child.Fd()), &opened); err != nil {
		child.Close()
		return nil, err
	}
	if opened.Dev != stat.Dev || opened.Ino != stat.Ino {
		child.Close()
		return nil, ErrUnsafeAssignmentPath
	}
	return child, nil
}

type ownedDirectoryEntry struct {
	path string
	dev  uint64
	ino  uint64
}

func makeOwnedDirectoriesWritable(ctx context.Context, root *os.File) error {
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil {
		return err
	}
	pending := []ownedDirectoryEntry{{dev: uint64(rootStat.Dev), ino: rootStat.Ino}}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		directory := root
		if current.path != "" {
			var err error
			directory, err = reopenOwnedDirectory(root, current.path)
			if err != nil {
				return err
			}
		}
		err := makeOwnedDirectoryWritable(ctx, directory, current, &pending)
		if directory != root {
			closeErr := directory.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func reopenOwnedDirectory(root *os.File, relative string) (*os.File, error) {
	parent := root
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if parent != root {
				parent.Close()
			}
			return nil, err
		}
		child, err := openOwnedDirectoryAtNoFollow(int(parent.Fd()), name, stat)
		if parent != root {
			parent.Close()
		}
		if err != nil {
			return nil, err
		}
		parent = child
	}
	return parent, nil
}

func makeOwnedDirectoryWritable(ctx context.Context, directory *os.File, current ownedDirectoryEntry, pending *[]ownedDirectoryEntry) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() ||
		uint64(stat.Dev) != current.dev || stat.Ino != current.ino {
		return fmt.Errorf("%w: assignment directory is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	mode := stat.Mode & 0o777
	if mode&0o700 != 0o700 {
		if err := unix.Fchmod(int(directory.Fd()), uint32(mode|0o700)); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, readErr := directory.Readdirnames(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			var childStat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), name, &childStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if childStat.Mode&unix.S_IFMT != unix.S_IFDIR {
				continue
			}
			path := name
			if current.path != "" {
				path = filepath.Join(current.path, name)
			}
			*pending = append(*pending, ownedDirectoryEntry{path: path, dev: uint64(childStat.Dev), ino: childStat.Ino})
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}
