//go:build linux

package workspace

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// O_PATH pins an unreadable directory without traversing a symlink. The
// procfd path names that pinned inode, even on kernels without fchmodat2.
func restoreUnreadableMiseDirectory(parentFD int, name string, expected unix.Stat_t) error {
	fd, err := unix.Openat(parentFD, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var pinned unix.Stat_t
	if err := unix.Fstat(fd, &pinned); err != nil {
		return err
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFDIR || int(pinned.Uid) != os.Geteuid() ||
		pinned.Dev != expected.Dev || pinned.Ino != expected.Ino {
		return ErrUnsafeAssignmentPath
	}
	return unix.Fchmodat(unix.AT_FDCWD, "/proc/self/fd/"+strconv.Itoa(fd), uint32(pinned.Mode&0o777|0o700), 0)
}
