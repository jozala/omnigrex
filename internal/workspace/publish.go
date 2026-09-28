package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

var (
	ErrInvalidPublication        = errors.New("invalid publication request")
	ErrPushRejected              = errors.New("publication push rejected")
	ErrTreeMismatch              = errors.New("workspace and publication trees differ")
	ErrUnexpectedHead            = errors.New("unexpected remote branch head")
	ErrUncommittedWorkspace      = errors.New("commit all workspace changes before publishing")
	ErrInvalidPublicationHistory = errors.New("publication history is not anchored to the scoped branch and default branch")
)

// UnexpectedHeadError contains only validated Git object IDs and the branch
// ref; Git command output and credentials are never retained in this error.
type UnexpectedHeadError struct {
	Expected string
	Actual   string
	Branch   string
}

func (err *UnexpectedHeadError) Error() string { return ErrUnexpectedHead.Error() + ": " + err.Branch }
func (err *UnexpectedHeadError) Unwrap() error { return ErrUnexpectedHead }

type Publication struct {
	AssignmentID      string
	RepositoryURL     string
	Credential        string
	BaseRevision      string
	ExpectedOldHead   string
	Branch            string
	DefaultBranch     string
	Message           string
	RecordProposedTip func(context.Context, string) error
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
	if err := lifecycle.validateWorkspaceHistoryMetadata(ctx, paths.Workspace); err != nil {
		return PublicationResult{}, err
	}
	status, err := lifecycle.gitOutput(ctx, "inspect committed workspace", paths.Workspace, "",
		nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return PublicationResult{}, err
	}
	if status != "" {
		return PublicationResult{}, ErrUncommittedWorkspace
	}
	head, err := lifecycle.gitOutput(ctx, "read committed workspace head", paths.Workspace, "",
		nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return PublicationResult{}, err
	}
	head, err = normalizeObjectID(head)
	if err != nil {
		return PublicationResult{}, err
	}
	if err := lifecycle.expectRemoteHead(ctx, paths.Publication, publication.Credential, publication.Branch, expectedOldHead); err != nil {
		return PublicationResult{}, err
	}
	if head == baseRevision {
		equal, err := lifecycle.committedTreesEqual(ctx, paths.Workspace, paths.Publication)
		if err != nil {
			return PublicationResult{}, err
		}
		if !equal {
			return PublicationResult{}, ErrTreeMismatch
		}
		return PublicationResult{Head: baseRevision}, nil
	}
	if publication.RecordProposedTip == nil {
		return PublicationResult{}, ErrInvalidPublication
	}
	if err := lifecycle.git(ctx, "import proposed commits", paths.Publication, "",
		"-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-recurse-submodules", "--", paths.Workspace, head); err != nil {
		return PublicationResult{}, err
	}
	if err := lifecycle.validatePublicationHistory(ctx, paths.Publication, publication, baseRevision, head); err != nil {
		return PublicationResult{}, err
	}
	if err := lifecycle.verifyCommittedTree(ctx, paths.Workspace, paths.Publication, head); err != nil {
		return PublicationResult{}, err
	}
	if err := publication.RecordProposedTip(ctx, head); err != nil {
		return PublicationResult{}, fmt.Errorf("record proposed publication tip: %w", err)
	}
	if err := lifecycle.expectRemoteHead(ctx, paths.Publication, publication.Credential, publication.Branch, expectedOldHead); err != nil {
		return PublicationResult{}, err
	}
	branchRef := "refs/heads/" + publication.Branch
	lease := "--force-with-lease=" + branchRef + ":" + expectedOldHead
	if err := lifecycle.git(ctx, "push publication commit", paths.Publication, publication.Credential,
		"push", "--porcelain", lease, "origin", head+":"+branchRef); err != nil {
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
		return &UnexpectedHeadError{Expected: expected, Actual: actual, Branch: branchRef}
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
	if !validBranch(publication.Branch) || publication.DefaultBranch != "" && !validBranch(publication.DefaultBranch) {
		return "", "", ErrInvalidPublication
	}
	return base, expected, nil
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
