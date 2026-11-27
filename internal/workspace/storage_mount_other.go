//go:build !linux

package workspace

import "os"

// Destructive retention requires mount-identity validation on the supported Linux host.
func storageSameMount(_, _ *os.File) error            { return ErrUnsafeAssignmentPath }
func storageLeafSameMount(_ *os.File, _ string) error { return ErrUnsafeAssignmentPath }

func storageRenameNoReplace(_ *os.File, _ string, _ *os.File, _ string) error {
	return ErrUnsafeAssignmentPath
}
