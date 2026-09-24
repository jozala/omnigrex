//go:build !linux

package workspace

import "golang.org/x/sys/unix"

func restoreUnreadableOwnedDirectory(parentFD int, name string, expected unix.Stat_t) error {
	return unix.Fchmodat(parentFD, name, uint32(expected.Mode&0o777|0o700), unix.AT_SYMLINK_NOFOLLOW)
}
