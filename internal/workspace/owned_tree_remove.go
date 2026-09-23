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

type removalEntry struct {
	ownedDirectoryEntry
	expanded bool
}

// removeOwnedDirectoryTree removes an already-detached, owned tree with a
// bounded number of open descriptors, never following an agent-created link.
func removeOwnedDirectoryTree(ctx context.Context, path string) error {
	root, _, err := openDirectoryNoFollow(path)
	if err != nil {
		return err
	}
	defer root.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil {
		return err
	}
	pending := []removalEntry{{ownedDirectoryEntry: ownedDirectoryEntry{dev: uint64(rootStat.Dev), ino: rootStat.Ino}}}
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current.expanded {
			if current.path == "" {
				continue
			}
			parent, name, err := removalParent(root, current.path)
			if err != nil {
				return err
			}
			var stat unix.Stat_t
			err = unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
			if err == nil && (stat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stat.Dev) != current.dev || stat.Ino != current.ino || int(stat.Uid) != os.Geteuid()) {
				err = ErrUnsafeAssignmentPath
			}
			if err == nil {
				err = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
			}
			if parent != root {
				parent.Close()
			}
			if err != nil {
				return err
			}
			continue
		}
		directory := root
		if current.path != "" {
			directory, err = reopenOwnedDirectory(root, current.path)
			if err != nil {
				return err
			}
		}
		var stat unix.Stat_t
		err = unix.Fstat(int(directory.Fd()), &stat)
		if err == nil && (uint64(stat.Dev) != current.dev || stat.Ino != current.ino || int(stat.Uid) != os.Geteuid()) {
			err = ErrUnsafeAssignmentPath
		}
		var children []removalEntry
		if err == nil {
			children, err = unlinkOwnedDirectoryFiles(ctx, directory, current.path)
		}
		if directory != root {
			closeErr := directory.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return err
		}
		current.expanded = true
		pending = append(pending, current)
		pending = append(pending, children...)
	}
	parent, _, err := openDirectoryNoFollow(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(path)
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if uint64(stat.Dev) != uint64(rootStat.Dev) || stat.Ino != rootStat.Ino || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafeAssignmentPath
	}
	return unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
}

func removalParent(root *os.File, relative string) (*os.File, string, error) {
	directory, name := filepath.Split(relative)
	if directory == "" {
		return root, name, nil
	}
	parent, err := reopenOwnedDirectory(root, strings.TrimSuffix(directory, string(filepath.Separator)))
	return parent, name, err
}

func unlinkOwnedDirectoryFiles(ctx context.Context, directory *os.File, relative string) ([]removalEntry, error) {
	children := make([]removalEntry, 0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, readErr := directory.Readdirnames(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return nil, err
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
					return nil, fmt.Errorf("unlink owned workspace file: %w", err)
				}
				continue
			}
			path := name
			if relative != "" {
				path = filepath.Join(relative, name)
			}
			children = append(children, removalEntry{ownedDirectoryEntry: ownedDirectoryEntry{
				path: path, dev: uint64(stat.Dev), ino: stat.Ino,
			}})
		}
		if errors.Is(readErr, io.EOF) {
			return children, nil
		}
	}
}
