package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrStaleMiseProvision = errors.New("assignment mise provisioning belongs to an older Agent Turn")

// The assignment root is shared by turns but is not mounted in the Runtime Process.
// This durable high-water mark prevents an old worker that waited for the file lock
// from replacing tool data installed by a newer execution epoch.
func claimMiseProvisionEpoch(ctx context.Context, root string, epoch int64, fence WorkspaceFence) error {
	if epoch < 0 || (epoch > 0) != (fence != nil) {
		return errors.New("mise provisioning epoch requires a matching Agent Turn fence")
	}
	marker := filepath.Join(root, ".mise-provision.epoch")
	latest, err := readMiseProvisionEpoch(marker)
	if err != nil {
		return err
	}
	if epoch < latest {
		return ErrStaleMiseProvision
	}
	claimed := false
	claim := func(fenceCtx context.Context) error {
		claimed = true
		if err := fenceCtx.Err(); err != nil {
			return err
		}
		if epoch == latest {
			return nil
		}
		file, err := os.CreateTemp(root, ".mise-provision-epoch-")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := file.WriteString(strconv.FormatInt(epoch, 10) + "\n"); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), marker)
	}
	if fence != nil {
		err = fence(ctx, claim)
	} else {
		err = claim(ctx)
	}
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("mise provisioning fence did not claim the execution epoch")
	}
	return nil
}

func readMiseProvisionEpoch(path string) (int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int(stat.Uid) != os.Geteuid() {
		return 0, ErrUnsafeAssignmentPath
	}
	content, err := io.ReadAll(io.LimitReader(file, 32))
	if err != nil {
		return 0, err
	}
	value := strings.TrimSuffix(string(content), "\n")
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil || epoch <= 0 || fmt.Sprintf("%d", epoch) != value {
		return 0, errors.New("invalid durable mise provisioning epoch")
	}
	return epoch, nil
}
