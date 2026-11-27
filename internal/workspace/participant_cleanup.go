package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrCleanupIncomplete = errors.New("Participant storage cleanup incomplete; retry required")

// StorageOutcome reports observations, not a promise of physical disk release.
// AllocatedBytes counts removed directories and the last observed link of files.
// Open handles, shared extents and concurrent host changes prevent physical accounting.
type StorageOutcome struct {
	ParticipantID  string `json:"participant_id"`
	Category       string `json:"category"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	AllocatedBytes int64  `json:"allocated_bytes_observed_removed"`
	Measurement    string `json:"measurement"`
}

type storageTarget struct{ root, name, category string }

// CleanupParticipantStorage requires the caller's irrevocable collection authority.
// Roots may coincide (as in Compose); only explicitly owned children are selected.
func (lifecycle *Lifecycle) CleanupParticipantStorage(ctx context.Context, id string) ([]StorageOutcome, error) {
	outcomes, err := lifecycle.cleanupParticipantStorage(ctx, id)
	if err != nil {
		slog.WarnContext(ctx, "Participant storage collection incomplete", "agent_participant_id", id, "category", "storage-validation-or-removal", "status", "incomplete", "reason", err.Error(), "measurement", "partial or unavailable; physical release unknown")
	}
	return outcomes, err
}

func (lifecycle *Lifecycle) cleanupParticipantStorage(ctx context.Context, id string) ([]StorageOutcome, error) {
	if _, err := lifecycle.Paths(id); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	roots := []struct{ path, category string }{{lifecycle.captureRoot, "captures"}, {lifecycle.miseRoot, "tools"}, {lifecycle.workspaceRoot, "workspace"}, {lifecycle.publicationRoot, "publication"}}
	targets := make([]storageTarget, 0)
	participant := "assignment-" + id
	seen := map[string]bool{}
	rootIdentities := map[[2]uint64]string{}
	backlog := false
	// Validate all root/target aliases before removing anything. A configured root
	// inside an owned Participant tree would otherwise grant unintended authority.
	for _, root := range roots[1:] {
		if lifecycle.captureRoot == root.path {
			return nil, ErrOverlappingPaths
		}
	}
	for _, a := range roots {
		for _, b := range roots {
			relative, err := filepath.Rel(filepath.Join(a.path, participant), b.path)
			if err != nil || relative == "." || relative != ".." && !startsWithParent(relative) && !filepath.IsAbs(relative) {
				return nil, ErrOverlappingPaths
			}
		}
	}
	for _, configured := range roots {
		key := filepath.Join(configured.path, participant)
		if seen[key] {
			continue
		}
		seen[key] = true
		parent, err := openStorageRoot(configured.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var rootStat unix.Stat_t
		if err := unix.Fstat(int(parent.Fd()), &rootStat); err != nil {
			parent.Close()
			return nil, err
		}
		identity := [2]uint64{uint64(rootStat.Dev), rootStat.Ino}
		if previous, exists := rootIdentities[identity]; exists && previous != configured.path {
			parent.Close()
			return nil, ErrOverlappingPaths
		}
		rootIdentities[identity] = configured.path
		owner, err := openStorageChild(parent, participant)
		parent.Close()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Capture trees have their own internal identity format. The exact outer
		// Participant directory is authoritative; no discovery by UUID-like filenames.
		if configured.category == "captures" {
			owner.Close()
			targets = append(targets, storageTarget{configured.path, participant, "captures"})
			continue
		}
		names, readErr := owner.Readdirnames(129)
		owner.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if len(names) > 128 {
			backlog = true
			names = names[:128]
		}
		for _, name := range names {
			category := ""
			for _, candidate := range roots {
				if candidate.path != configured.path {
					continue
				}
				switch candidate.category {
				case "tools":
					if name == "mise" || name == "tool-data" {
						category = name
					}
				case "workspace":
					if name == "workspace" {
						category = "workspace"
					}
					for _, prefix := range []string{"workspace-staged-", "workspace-retired-"} {
						if _, ok := workspaceDirectoryEpoch(name, prefix); ok {
							category = "workspace-staging"
						}
					}
				case "publication":
					if name == "publication" || strings.HasPrefix(name, ".inspect-") || strings.HasPrefix(name, ".corroborate-") {
						category = "publication"
					}
				}
			}
			if category == "" {
				return nil, fmt.Errorf("%w: unrecognized Participant storage entry", ErrUnsafeAssignmentPath)
			}
			targets = append(targets, storageTarget{key, name, category})
		}
	}
	// Include mandatory categories even when their optional backing roots are absent.
	mandatory := []storageTarget{{lifecycle.captureRoot, participant, "captures"}, {filepath.Join(lifecycle.miseRoot, participant), "mise", "mise"}, {filepath.Join(lifecycle.miseRoot, participant), "tool-data", "tool-data"}, {filepath.Join(lifecycle.workspaceRoot, participant), "workspace", "workspace"}, {filepath.Join(lifecycle.publicationRoot, participant), "publication", "publication"}}
	selected := map[string]bool{}
	for _, target := range targets {
		selected[filepath.Join(target.root, target.name)] = true
	}
	for _, target := range mandatory {
		if !selected[filepath.Join(target.root, target.name)] {
			targets = append(targets, target)
		}
	}
	outcomes := make([]StorageOutcome, 0, len(targets))
	budget := 8192
	for _, target := range targets {
		result := StorageOutcome{ParticipantID: id, Category: target.category, Status: "absent", Measurement: "observed-last-links; physical release unknown"}
		parent, err := openStorageRoot(target.root)
		if err == nil {
			err = removeStorageTree(ctx, parent, target.name, 0, &budget, &result.AllocatedBytes)
			parent.Close()
		}
		if os.IsNotExist(err) {
			err = nil
		}
		if err != nil {
			result.Status = "incomplete"
			result.Reason = err.Error()
		} else if result.AllocatedBytes > 0 {
			result.Status = "removed"
		}
		outcomes = append(outcomes, result)
		slog.InfoContext(ctx, "Participant storage collection", "agent_participant_id", id, "category", result.Category, "status", result.Status, "reason", result.Reason, "allocated_bytes_observed_removed", result.AllocatedBytes, "measurement", result.Measurement)
		if err != nil {
			return outcomes, err
		}
	}
	if backlog {
		return outcomes, ErrCleanupIncomplete
	}
	// Remove only empty Participant shells, never recursively sweep their parent.
	shell := StorageOutcome{ParticipantID: id, Category: "participant-directories", Status: "absent", Measurement: "observed-directory-blocks; physical release unknown"}
	seen = map[string]bool{}
	for _, configured := range roots {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		if seen[configured.path] {
			continue
		}
		seen[configured.path] = true
		parent, err := openStorageRoot(configured.path)
		if err == nil {
			var owner *os.File
			owner, err = openStorageChild(parent, participant)
			if err == nil {
				var opened, current unix.Stat_t
				err = unix.Fstat(int(owner.Fd()), &opened)
				if err == nil {
					err = unix.Fstatat(int(parent.Fd()), participant, &current, unix.AT_SYMLINK_NOFOLLOW)
				}
				if err == nil && (opened.Dev != current.Dev || opened.Ino != current.Ino) {
					err = ErrUnsafeAssignmentPath
				}
				if err == nil {
					err = unix.Unlinkat(int(parent.Fd()), participant, unix.AT_REMOVEDIR)
				}
				if err == nil {
					shell.AllocatedBytes += opened.Blocks * 512
				}
				owner.Close()
			}
			parent.Close()
		}
		if os.IsNotExist(err) {
			err = nil
		}
		if err != nil {
			shell.Status = "incomplete"
			shell.Reason = err.Error()
			return append(outcomes, shell), err
		}
	}
	outcomes = append(outcomes, shell)
	slog.InfoContext(ctx, "Participant storage collection", "agent_participant_id", id, "category", shell.Category, "status", shell.Status, "allocated_bytes_observed_removed", shell.AllocatedBytes, "measurement", shell.Measurement)
	return outcomes, nil
}

// Open every component without following aliases, including configured ancestors.
func openStorageRoot(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrUnsafeAssignmentPath
	}
	current, _, err := openDirectoryNoFollow("/")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if name == "" {
			continue
		}
		next, err := openDirectoryAtNoFollow(int(current.Fd()), name)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(current.Fd()), &stat); err != nil {
		current.Close()
		return nil, err
	}
	if int(stat.Uid) != os.Geteuid() {
		current.Close()
		return nil, ErrUnsafeAssignmentPath
	}
	return current, nil
}

func openStorageChild(parent *os.File, name string) (*os.File, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if err := storageLeafSameMount(parent, name); err != nil {
		return nil, err
	}
	child, err := openOwnedDirectoryAtNoFollow(int(parent.Fd()), name, stat)
	if err != nil {
		return nil, err
	}
	if err = storageSameMount(parent, child); err != nil {
		child.Close()
		return nil, err
	}
	if err = unix.Fchmod(int(child.Fd()), uint32(stat.Mode&0o777|0o700)); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

// Depth, enumeration buffers, descriptors, work and time are bounded. Removed
// entries are durable progress; a fresh attempt starts at the remaining tree.
func removeStorageTree(ctx context.Context, parent *os.File, name string, depth int, budget *int, bytes *int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= 32 || *budget <= 0 {
		return ErrCleanupIncomplete
	}
	*budget--
	directory, err := openStorageChild(parent, name)
	if err != nil {
		return err
	}
	defer directory.Close()
	var original unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &original); err != nil {
		return err
	}
	for {
		names, readErr := directory.Readdirnames(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, child := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if *budget <= 0 {
				return ErrCleanupIncomplete
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(int(directory.Fd()), child, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if stat.Dev != original.Dev || int(stat.Uid) != os.Geteuid() {
				return ErrUnsafeAssignmentPath
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				if err := removeStorageTree(ctx, directory, child, depth+1, budget, bytes); err != nil {
					return err
				}
			} else {
				*budget--
				// Leaf links are explicitly unlinkable within this exclusively owned tree;
				// chmod and traversal never follow them. Special files are not accepted.
				kind := stat.Mode & unix.S_IFMT
				if kind != unix.S_IFREG && kind != unix.S_IFLNK {
					return ErrUnsafeAssignmentPath
				}
				if err := storageLeafSameMount(directory, child); err != nil {
					return err
				}
				if err := unix.Unlinkat(int(directory.Fd()), child, 0); err != nil {
					return err
				}
				if stat.Nlink == 1 {
					*bytes += stat.Blocks * 512
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	var final unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &final, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if final.Dev != original.Dev || final.Ino != original.Ino {
		return ErrUnsafeAssignmentPath
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	*bytes += original.Blocks * 512
	return nil
}

// RemoveRetainedStorageTree is the backend retained-state collection primitive.
// The caller must validate its exact state binding and hold collection exclusion.
// Only categories whose existing contract permits a root link may unlink it;
// no link target is traversed. Docker may additionally confirm daemon absence.
func RemoveRetainedStorageTree(ctx context.Context, path string, allowRootLink bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := openStorageRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(path)
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK && allowRootLink {
		if int(stat.Uid) != os.Geteuid() {
			return ErrUnsafeAssignmentPath
		}
		if err := storageLeafSameMount(parent, name); err != nil {
			return err
		}
		return unix.Unlinkat(int(parent.Fd()), name, 0)
	}
	budget := 8192
	var bytes int64
	return removeStorageTree(ctx, parent, name, 0, &budget, &bytes)
}

// ValidateRetainedStorageRoot verifies the configured backend mount even when a
// Participant tree is already absent after partial collection. An alias or a
// missing global root is not evidence that the original Participant data is gone.
func ValidateRetainedStorageRoot(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := openStorageRoot(path)
	if err != nil {
		return err
	}
	return root.Close()
}
