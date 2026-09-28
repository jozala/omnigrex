package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jozala/omnigrex/internal/turnconfig"
	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

const TurnPathMount = "/home/opencode/.local/share/omnigrex-tool-data"

func (lifecycle *Lifecycle) toolDataRoot(assignmentID string) (string, error) {
	if !uuidtext.Valid(assignmentID) {
		return "", ErrInvalidAssignmentID
	}
	return filepath.Join(lifecycle.miseRoot, "assignment-"+assignmentID, "tool-data"), nil
}

// PrepareTurnPaths creates only assignment-owned paths under the existing disk-backed mise volume.
func (lifecycle *Lifecycle) PrepareTurnPaths(ctx context.Context, assignmentID, turnID string, configuration turnconfig.Configuration, fence WorkspaceFence) (map[string]string, error) {
	if !uuidtext.Valid(turnID) || lifecycle == nil {
		return nil, ErrUnsafeAssignmentPath
	}
	root, err := lifecycle.toolDataRoot(assignmentID)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(configuration.Environment))
	prepare := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ensureAssignmentDirectory(lifecycle.miseRoot, filepath.Dir(root)); err != nil {
			return err
		}
		if err := ensureOwnedDirectory(root, 0o700); err != nil {
			return err
		}
		if err := makeOwnedToolParentsWritable(filepath.Dir(root), "tool-data"); err != nil {
			return err
		}
		for name, lifetime := range configuration.Directories {
			if lifetime != turnconfig.Turn && lifetime != turnconfig.Assignment || !turnconfig.ValidDirectory(name) {
				return turnconfig.ErrInvalid
			}
			parent := filepath.Join(root, string(lifetime))
			if err := makeOwnedToolParentsWritable(filepath.Dir(root), "tool-data", string(lifetime)); err != nil {
				return err
			}
			if err := ensureOwnedDirectory(parent, 0o700); err != nil {
				return err
			}
			if lifetime == turnconfig.Turn {
				parent = filepath.Join(parent, turnID)
				if err := makeOwnedToolParentsWritable(filepath.Dir(root), "tool-data", "turn", turnID); err != nil {
					return err
				}
				if err := ensureOwnedDirectory(parent, 0o700); err != nil {
					return err
				}
			}
			if err := ensureOwnedDirectory(filepath.Join(parent, name), 0o700); err != nil {
				return err
			}
		}
		return nil
	}
	if fence != nil {
		err = fence(ctx, prepare)
	} else {
		err = prepare(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("prepare Agent Turn tool paths: %w", err)
	}
	for name, directory := range configuration.Environment {
		lifetime, exists := configuration.Directories[directory]
		if !exists || !turnconfig.SafeVariable(name) {
			return nil, turnconfig.ErrInvalid
		}
		parts := []string{TurnPathMount, string(lifetime)}
		if lifetime == turnconfig.Turn {
			parts = append(parts, turnID)
		}
		values[name] = filepath.Join(append(parts, directory)...)
	}
	return values, nil
}

// CleanupTurnPaths removes the exact stopped turn's scratch directory; caches remain until Assignment collection.
func (lifecycle *Lifecycle) CleanupTurnPaths(ctx context.Context, assignmentID, turnID string, fence WorkspaceFence) error {
	if lifecycle == nil || !uuidtext.Valid(turnID) {
		return ErrUnsafeAssignmentPath
	}
	root, err := lifecycle.toolDataRoot(assignmentID)
	if err != nil {
		return err
	}
	remove := func(ctx context.Context) error {
		path := filepath.Join(root, "turn", turnID)
		for _, parent := range []string{lifecycle.miseRoot, filepath.Dir(root)} {
			exists, err := inspectOwnedDirectory(parent)
			if err != nil || !exists {
				return err
			}
		}
		if err := makeOwnedToolParentsWritable(filepath.Dir(root), "tool-data", "turn"); err != nil {
			return err
		}
		for _, parent := range []string{root, filepath.Dir(path)} {
			exists, err := inspectOwnedDirectory(parent)
			if err != nil || !exists {
				return err
			}
		}
		exists, err := inspectOwnedDirectory(path)
		if err != nil || !exists {
			return err
		}
		if err := makeOwnedTreeDirectoriesWritable(ctx, path); err != nil {
			return err
		}
		return removeOwnedDirectoryTree(ctx, path)
	}
	if fence != nil {
		return fence(ctx, remove)
	}
	return remove(ctx)
}

// Restore only path ancestors, not every cached file, before traversing agent-writable directories.
// Each component is opened and checked by inode without following links.
func makeOwnedToolParentsWritable(assignmentRoot string, names ...string) error {
	parent, _, err := openDirectoryNoFollow(assignmentRoot)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	for _, name := range names {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		child, err := openOwnedDirectoryAtNoFollow(int(parent.Fd()), name, stat)
		if err != nil {
			return err
		}
		var opened unix.Stat_t
		if err := unix.Fstat(int(child.Fd()), &opened); err != nil {
			child.Close()
			return err
		}
		if opened.Mode&0o700 != 0o700 {
			if err := unix.Fchmod(int(child.Fd()), uint32(opened.Mode&0o777|0o700)); err != nil {
				child.Close()
				return err
			}
		}
		_ = parent.Close()
		parent = child
	}
	return nil
}

// CleanupAssignmentToolPaths removes cached and orphaned scratch data after Assignment retention.
func (lifecycle *Lifecycle) CleanupAssignmentToolPaths(ctx context.Context, assignmentID string) error {
	root, err := lifecycle.toolDataRoot(assignmentID)
	if err != nil {
		return err
	}
	for _, parent := range []string{lifecycle.miseRoot, filepath.Dir(root)} {
		exists, err := inspectOwnedDirectory(parent)
		if err != nil || !exists {
			return err
		}
	}
	exists, err := inspectOwnedDirectory(root)
	if err != nil || !exists {
		return err
	}
	if err := makeOwnedTreeDirectoriesWritable(ctx, root); err != nil {
		return err
	}
	if err := removeOwnedDirectoryTree(ctx, root); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
