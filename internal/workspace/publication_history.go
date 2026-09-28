package workspace

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (lifecycle *Lifecycle) validateWorkspaceHistoryMetadata(ctx context.Context, directory string) error {
	gitDirectory := filepath.Join(directory, ".git")
	info, err := os.Lstat(gitDirectory)
	if err != nil || !info.IsDir() {
		return ErrInvalidPublicationHistory
	}
	for _, path := range []string{"info/grafts", "shallow"} {
		_, err := os.Lstat(filepath.Join(gitDirectory, path))
		if err == nil || !os.IsNotExist(err) {
			return ErrInvalidPublicationHistory
		}
	}
	replacements, err := lifecycle.gitOutput(ctx, "inspect workspace replacement refs", directory, "", nil,
		"for-each-ref", "--format=%(refname)", "refs/replace")
	if err != nil {
		return err
	}
	if replacements != "" {
		return ErrInvalidPublicationHistory
	}
	return nil
}

// validatePublicationHistory uses the trusted clone's original commit objects.
// No agent-provided refs, replace objects, or repository configuration can
// authorize a merge parent.
func (lifecycle *Lifecycle) validatePublicationHistory(ctx context.Context, directory string, publication Publication, base, head string) error {
	parents, err := lifecycle.gitOutput(ctx, "inspect proposed commit ancestry", directory, "", nil,
		"rev-list", "--parents", "--first-parent", head)
	if err != nil {
		return err
	}
	var mainHead string
	reachedBase := false
	for _, line := range strings.Split(parents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 || len(fields) > 3 {
			return ErrInvalidPublicationHistory
		}
		if fields[0] == base {
			reachedBase = true
			break
		}
		if len(fields) < 2 {
			return ErrInvalidPublicationHistory
		}
		if len(fields) == 3 {
			if publication.DefaultBranch == "" {
				return ErrInvalidPublicationHistory
			}
			if mainHead == "" {
				ref := "refs/heads/" + publication.DefaultBranch
				if err := lifecycle.git(ctx, "fetch trusted default branch", directory, publication.Credential,
					"fetch", "--force", "--no-tags", "origin", ref+":refs/remotes/origin/"+publication.DefaultBranch); err != nil {
					return err
				}
				mainHead, err = lifecycle.gitOutput(ctx, "resolve trusted default branch", directory, "", nil,
					"rev-parse", "--verify", "refs/remotes/origin/"+publication.DefaultBranch+"^{commit}")
				if err != nil {
					return err
				}
			}
			if err := lifecycle.git(ctx, "verify default branch merge parent", directory, "",
				"merge-base", "--is-ancestor", fields[2], mainHead); err != nil {
				return ErrInvalidPublicationHistory
			}
		}
		entries, err := lifecycle.gitOutput(ctx, "inspect published commit tree", directory, "", nil,
			"ls-tree", "-rz", "--full-tree", fields[0])
		if err != nil {
			return err
		}
		for _, entry := range strings.Split(entries, "\x00") {
			if entry == "" {
				continue
			}
			metadata, path, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(metadata)
			if !ok || len(fields) != 3 || fields[1] != "blob" ||
				fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000" {
				return ErrInvalidPublicationHistory
			}
			for _, segment := range strings.Split(path, "/") {
				if segment == ".git" {
					return ErrInvalidPublicationHistory
				}
			}
		}
	}
	if !reachedBase {
		return ErrInvalidPublicationHistory
	}
	return nil
}

func (lifecycle *Lifecycle) verifyCommittedTree(ctx context.Context, workspaceDirectory, publicationDirectory, head string) error {
	if err := lifecycle.git(ctx, "check out validated publication tip", publicationDirectory, "",
		"checkout", "--detach", "--force", head); err != nil {
		return err
	}
	equal, err := lifecycle.committedTreesEqual(ctx, workspaceDirectory, publicationDirectory)
	if err != nil {
		return err
	}
	if !equal {
		return ErrTreeMismatch
	}
	return nil
}

// CommittedTreesEqual corroborates the published commit and the clean agent
// workspace without treating ignored local build artifacts as publications.
func CommittedTreesEqual(ctx context.Context, paths Paths) (bool, error) {
	lifecycle := &Lifecycle{gitExecutable: "git"}
	return lifecycle.committedTreesEqual(ctx, paths.Workspace, paths.Publication)
}

func (lifecycle *Lifecycle) committedTreesEqual(ctx context.Context, workspaceDirectory, publicationDirectory string) (bool, error) {
	status, err := lifecycle.gitOutput(ctx, "inspect published workspace status", workspaceDirectory, "", nil,
		"status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	if status != "" {
		return false, nil
	}
	workspaceHead, err := lifecycle.gitOutput(ctx, "inspect published workspace head", workspaceDirectory, "", nil,
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return false, err
	}
	publicationHead, err := lifecycle.gitOutput(ctx, "inspect publication head", publicationDirectory, "", nil,
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return false, err
	}
	if workspaceHead != publicationHead {
		return false, nil
	}
	tracked, err := lifecycle.gitOutput(ctx, "list tracked workspace entries", workspaceDirectory, "", nil,
		"ls-files", "-z", "--cached", "--full-name")
	if err != nil {
		return false, err
	}
	trackedPaths := make([]string, 0)
	for _, path := range strings.Split(tracked, "\x00") {
		if path != "" {
			trackedPaths = append(trackedPaths, path)
		}
	}
	workspaceTree, err := snapshotTrackedTree(workspaceDirectory, trackedPaths)
	if err != nil {
		return false, err
	}
	publicationTree, err := SnapshotTree(publicationDirectory)
	if err != nil {
		return false, err
	}
	sort.Slice(workspaceTree.entries, func(left, right int) bool {
		return workspaceTree.entries[left].path < workspaceTree.entries[right].path
	})
	sort.Slice(publicationTree.entries, func(left, right int) bool {
		return publicationTree.entries[left].path < publicationTree.entries[right].path
	})
	if !workspaceTree.Equal(publicationTree) {
		return false, nil
	}
	return true, nil
}

func (lifecycle *Lifecycle) verifyFirstParentBase(ctx context.Context, directory, base, proposed string) error {
	chain, err := lifecycle.gitOutput(ctx, "inspect publication first-parent chain", directory, "", nil,
		"rev-list", "--first-parent", proposed)
	if err != nil {
		return err
	}
	for _, revision := range strings.Fields(chain) {
		if revision == base {
			return nil
		}
	}
	return ErrInvalidPublicationHistory
}
