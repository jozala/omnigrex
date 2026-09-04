package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

var (
	ErrInvalidPublication = errors.New("invalid publication request")
	ErrPushRejected       = errors.New("publication push rejected")
	ErrTreeMismatch       = errors.New("workspace and publication trees differ")
	ErrUnexpectedHead     = errors.New("unexpected remote branch head")
)

type CommitIdentity struct {
	Name  string
	Email string
}

type Publication struct {
	AssignmentID    string
	RepositoryURL   string
	Credential      string
	BaseRevision    string
	ExpectedOldHead string
	Branch          string
	Message         string
	Identity        CommitIdentity
	Time            time.Time
}

type PublicationResult struct {
	Head    string
	Changed bool
}

func (lifecycle *Lifecycle) Publish(ctx context.Context, publication Publication) (PublicationResult, error) {
	paths, err := lifecycle.Paths(publication.AssignmentID)
	if err != nil {
		return PublicationResult{}, err
	}
	baseRevision, expectedOldHead, err := validatePublication(publication)
	if err != nil {
		return PublicationResult{}, err
	}
	release := lifecycle.acquirePublicationLock(publication.AssignmentID)
	defer release()

	if err := lifecycle.preparePublication(ctx, paths.Publication, publication, baseRevision); err != nil {
		return PublicationResult{}, err
	}
	baseTree, err := SnapshotTree(paths.Publication)
	if err != nil {
		return PublicationResult{}, err
	}
	workspaceTree, err := SnapshotTree(paths.Workspace)
	if err != nil {
		return PublicationResult{}, err
	}
	if err := SyncTree(paths.Workspace, paths.Publication); err != nil {
		return PublicationResult{}, err
	}
	publicationTree, err := SnapshotTree(paths.Publication)
	if err != nil {
		return PublicationResult{}, err
	}
	if !workspaceTree.Equal(publicationTree) {
		return PublicationResult{}, ErrTreeMismatch
	}
	if err := lifecycle.expectRemoteHead(ctx, paths.Publication, publication.Credential, publication.Branch, expectedOldHead); err != nil {
		return PublicationResult{}, err
	}
	if baseTree.Equal(workspaceTree) {
		return PublicationResult{Head: baseRevision}, nil
	}
	if err := lifecycle.git(ctx, "stage publication tree", paths.Publication, "",
		"add", "--all", "--force", "--", "."); err != nil {
		return PublicationResult{}, err
	}
	instant := publication.Time.UTC().Format(time.RFC3339)
	identity := map[string]string{
		"GIT_AUTHOR_NAME": publication.Identity.Name, "GIT_AUTHOR_EMAIL": publication.Identity.Email, "GIT_AUTHOR_DATE": instant,
		"GIT_COMMITTER_NAME": publication.Identity.Name, "GIT_COMMITTER_EMAIL": publication.Identity.Email, "GIT_COMMITTER_DATE": instant,
	}
	if _, err := lifecycle.gitOutput(ctx, "commit publication tree", paths.Publication, "", identity,
		"commit", "--no-verify", "--no-gpg-sign", "-m", publication.Message); err != nil {
		return PublicationResult{}, err
	}
	head, err := lifecycle.gitOutput(ctx, "resolve publication commit", paths.Publication, "", nil,
		"rev-parse", "--verify", "HEAD")
	if err != nil {
		return PublicationResult{}, err
	}
	head, err = normalizeObjectID(head)
	if err != nil {
		return PublicationResult{}, fmt.Errorf("resolve publication commit: %w", err)
	}
	if err := lifecycle.expectRemoteHead(ctx, paths.Publication, publication.Credential, publication.Branch, expectedOldHead); err != nil {
		return PublicationResult{}, err
	}
	branchRef := "refs/heads/" + publication.Branch
	lease := "--force-with-lease=" + branchRef + ":" + expectedOldHead
	if err := lifecycle.git(ctx, "push publication commit", paths.Publication, publication.Credential,
		"push", "--porcelain", lease, "origin", "HEAD:"+branchRef); err != nil {
		return PublicationResult{}, fmt.Errorf("%w: %v", ErrPushRejected, err)
	}
	return PublicationResult{Head: head, Changed: true}, nil
}

func (lifecycle *Lifecycle) preparePublication(ctx context.Context, destination string, publication Publication, baseRevision string) (err error) {
	if err := ensureAssignmentDirectory(lifecycle.publicationRoot, filepath.Dir(destination)); err != nil {
		return fmt.Errorf("create publication parent: %w", err)
	}
	if _, err := inspectOwnedDirectory(destination); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return fmt.Errorf("replace publication checkout: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	if err = lifecycle.git(ctx, "clone publication repository", "", publication.Credential,
		"clone", "--no-checkout", "--origin=origin", "--config", "core.hooksPath=/dev/null", "--", publication.RepositoryURL, destination); err != nil {
		return err
	}
	if err = lifecycle.git(ctx, "set publication remote", destination, "",
		"remote", "set-url", "origin", publication.RepositoryURL); err != nil {
		return err
	}
	if err = lifecycle.git(ctx, "disable publication hooks", destination, "",
		"config", "--local", "core.hooksPath", os.DevNull); err != nil {
		return err
	}
	if err = lifecycle.git(ctx, "fetch publication base", destination, publication.Credential,
		"fetch", "--force", "--no-tags", "origin", baseRevision); err != nil {
		return err
	}
	if err = lifecycle.git(ctx, "check out publication base", destination, "",
		"checkout", "--detach", "--force", baseRevision); err != nil {
		return err
	}
	return lifecycle.git(ctx, "reset publication base", destination, "",
		"reset", "--hard", baseRevision)
}

func (lifecycle *Lifecycle) expectRemoteHead(ctx context.Context, directory, credential, branch, expected string) error {
	branchRef := "refs/heads/" + branch
	output, err := lifecycle.gitOutput(ctx, "read remote branch head", directory, credential, nil,
		"ls-remote", "--refs", "origin", branchRef)
	if err != nil {
		return err
	}
	actual := ""
	if output != "" {
		fields := strings.Fields(output)
		if len(fields) != 2 || fields[1] != branchRef {
			return fmt.Errorf("read remote branch head: invalid response")
		}
		actual, err = normalizeObjectID(fields[0])
		if err != nil {
			return fmt.Errorf("read remote branch head: invalid object ID")
		}
	}
	if actual != expected {
		return fmt.Errorf("%w: %s", ErrUnexpectedHead, branchRef)
	}
	return nil
}

func validatePublication(publication Publication) (string, string, error) {
	if err := validateRemote(publication.RepositoryURL); err != nil {
		return "", "", err
	}
	if err := validateCredential(publication.Credential); err != nil {
		return "", "", err
	}
	base, err := normalizeObjectID(publication.BaseRevision)
	if err != nil {
		return "", "", err
	}
	expected := ""
	if publication.ExpectedOldHead != "" {
		expected, err = normalizeObjectID(publication.ExpectedOldHead)
		if err != nil {
			return "", "", err
		}
		if expected != base {
			return "", "", fmt.Errorf("%w: expected old head must equal publication base", ErrInvalidPublication)
		}
	}
	if !validBranch(publication.Branch) || strings.TrimSpace(publication.Message) == "" || publication.Time.IsZero() ||
		!validIdentity(publication.Identity.Name) || !validIdentity(publication.Identity.Email) {
		return "", "", ErrInvalidPublication
	}
	return base, expected, nil
}

func validIdentity(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validBranch(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || value == "@" || strings.HasPrefix(value, "-") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.Contains(value, "//") || strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || strings.ContainsRune("~^:?*[", character) {
			return false
		}
	}
	return true
}
