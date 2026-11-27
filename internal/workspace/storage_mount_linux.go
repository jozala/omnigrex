package workspace

import (
	"golang.org/x/sys/unix"
	"os"
)

func storageMount(fd int, name string, flags int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(fd, name, flags, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, ErrUnsafeAssignmentPath
	}
	return stat.Mnt_id, nil
}
func storageSameMount(parent, child *os.File) error {
	a, err := storageMount(int(parent.Fd()), "", unix.AT_EMPTY_PATH)
	if err != nil {
		return err
	}
	b, err := storageMount(int(child.Fd()), "", unix.AT_EMPTY_PATH)
	if err != nil {
		return err
	}
	if a != b {
		return ErrUnsafeAssignmentPath
	}
	return nil
}
func storageLeafSameMount(parent *os.File, name string) error {
	a, err := storageMount(int(parent.Fd()), "", unix.AT_EMPTY_PATH)
	if err != nil {
		return err
	}
	b, err := storageMount(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if a != b {
		return ErrUnsafeAssignmentPath
	}
	return nil
}
