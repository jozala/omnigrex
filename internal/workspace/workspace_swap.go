package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WorkspaceFence holds durable Agent Turn authority for a short filesystem transition.
type WorkspaceFence func(context.Context, func(context.Context) error) error

func swapAssignmentWorkspace(ctx context.Context, active, staged string, epoch int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	exists, err := inspectOwnedDirectory(active)
	if err != nil {
		return "", err
	}
	if _, err := inspectOwnedDirectory(staged); err != nil {
		return "", err
	}
	var detached string
	if exists {
		detached, err = reserveDetachedWorkspacePath(filepath.Dir(active), epoch)
		if err != nil {
			return "", err
		}
		if err := os.Rename(active, detached); err != nil {
			return "", fmt.Errorf("detach assignment workspace: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Join(err, restoreDetachedWorkspace(active, detached))
	}
	if err := os.Rename(staged, active); err != nil {
		return "", errors.Join(fmt.Errorf("activate staged assignment workspace: %w", err), restoreDetachedWorkspace(active, detached))
	}
	return detached, nil
}

func detachAssignmentWorkspace(ctx context.Context, active string, epoch int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	exists, err := inspectOwnedDirectory(active)
	if err != nil || !exists {
		return "", err
	}
	detached, err := reserveDetachedWorkspacePath(filepath.Dir(active), epoch)
	if err != nil {
		return "", err
	}
	if err := os.Rename(active, detached); err != nil {
		return "", fmt.Errorf("detach assignment workspace: %w", err)
	}
	return detached, nil
}

func reserveDetachedWorkspacePath(root string, epoch int64) (string, error) {
	path, err := os.MkdirTemp(root, "workspace-retired-"+strconv.FormatInt(epoch, 10)+"-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func restoreDetachedWorkspace(active, detached string) error {
	if detached == "" {
		return nil
	}
	if err := os.Rename(detached, active); err != nil {
		return fmt.Errorf("restore detached assignment workspace: %w", err)
	}
	return nil
}

func removeOwnedWorkspaceTree(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := inspectOwnedDirectory(path)
	if err != nil || !exists {
		return err
	}
	if err := makeOwnedTreeDirectoriesWritable(ctx, path); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return removeOwnedDirectoryTree(ctx, path)
}

func cleanupDetachedWorkspace(ctx context.Context, path string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return removeOwnedWorkspaceTree(cleanupCtx, path)
}

// Cleanup for this epoch is mandatory for Reviewer discard; a retry must not
// claim success while a detached workspace from its own turn remains.
func cleanupRetiredWorkspacesForEpoch(ctx context.Context, assignmentRoot string, epoch int64) error {
	entries, err := os.ReadDir(assignmentRoot)
	if err != nil {
		return err
	}
	prefix := "workspace-retired-" + strconv.FormatInt(epoch, 10) + "-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			if err := removeOwnedWorkspaceTree(ctx, filepath.Join(assignmentRoot, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// An older epoch's staged or retired tree cannot belong to a successor.
// Cleanup has one time budget, reports failures, and never visits a newer epoch.
func cleanupOldEpochDirectories(ctx context.Context, assignmentRoot, prefix string, epoch int64) error {
	if epoch <= 0 {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	entries, err := os.ReadDir(assignmentRoot)
	if err != nil {
		return err
	}
	const maximumDirectoriesPerPass = 4
	processed := 0
	for _, entry := range entries {
		if err := cleanupCtx.Err(); err != nil {
			return err
		}
		oldEpoch, found := workspaceDirectoryEpoch(entry.Name(), prefix)
		if found && oldEpoch < epoch {
			if processed >= maximumDirectoriesPerPass {
				return fmt.Errorf("older %s workspace cleanup backlog remains", prefix)
			}
			processed++
			if err := removeOwnedWorkspaceTree(cleanupCtx, filepath.Join(assignmentRoot, entry.Name())); err != nil {
				return fmt.Errorf("remove older %s workspace: %w", prefix, err)
			}
		}
	}
	return nil
}

func workspaceDirectoryEpoch(name, prefix string) (int64, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	epochText, _, found := strings.Cut(strings.TrimPrefix(name, prefix), "-")
	if !found {
		return 0, false
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	return epoch, err == nil && epoch >= 0
}
