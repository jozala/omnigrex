package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Go's module cache contains read-only directories. Restore only the owner's
// directory permissions, using no-follow descriptors so an agent-created link
// cannot redirect chmod outside its assignment data.
func makeOwnedTreeDirectoriesWritable(path string) error {
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
	return makeOwnedDirectoryWritable(directory)
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

func makeOwnedDirectoryWritable(directory *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: assignment directory is not owned by the orchestrator", ErrUnsafeAssignmentPath)
	}
	mode := stat.Mode & 0o777
	if mode&0o700 != 0o700 {
		if err := unix.Fchmod(int(directory.Fd()), uint32(mode|0o700)); err != nil {
			return err
		}
	}
	for {
		names, readErr := directory.Readdirnames(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, name := range names {
			var childStat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), name, &childStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if childStat.Mode&unix.S_IFMT != unix.S_IFDIR {
				continue
			}
			child, err := openOwnedDirectoryAtNoFollow(int(directory.Fd()), name, childStat)
			if err != nil {
				return err
			}
			err = makeOwnedDirectoryWritable(child)
			closeErr := child.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}
