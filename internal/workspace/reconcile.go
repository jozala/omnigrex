package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrInvalidPublicationReconciliation = errors.New("invalid publication reconciliation request")

// PublicationReconciliationOutcome describes what the exact remote branch proves about a publication attempt.
type PublicationReconciliationOutcome string

const (
	PublicationReconciliationFound   PublicationReconciliationOutcome = "FOUND"
	PublicationReconciliationAbsent  PublicationReconciliationOutcome = "ABSENT"
	PublicationReconciliationUnknown PublicationReconciliationOutcome = "UNKNOWN"
)

// PublicationReconciliation identifies one possibly published commit and its branch precondition.
type PublicationReconciliation struct {
	AssignmentID       string
	RepositoryURL      string
	Credential         string
	BaseRevision       string
	ProposedRevision   string
	HistoryPublication bool
	ExpectedOldHead    string
	Branch             string
	OperationID        string
}

// PublicationReconciliationResult reports the observed branch head, or the matching publication commit when found.
type PublicationReconciliationResult struct {
	Outcome PublicationReconciliationOutcome
	Head    string
}

// ReconcilePublication inspects a fresh orchestrator-owned clone for a publication operation.
func (lifecycle *Lifecycle) ReconcilePublication(ctx context.Context, input PublicationReconciliation) (PublicationReconciliationResult, error) {
	paths, err := lifecycle.Paths(input.AssignmentID)
	if err != nil {
		return PublicationReconciliationResult{}, err
	}
	base, expected, err := validatePublicationReconciliation(input)
	if err != nil {
		return PublicationReconciliationResult{}, err
	}
	release := lifecycle.acquirePublicationLock(input.AssignmentID)
	defer release()

	if err := lifecycle.prepareReconciliation(ctx, paths.Publication, input); err != nil {
		return PublicationReconciliationResult{}, err
	}
	actual, err := lifecycle.remoteBranchHead(ctx, paths.Publication, input.Credential, input.Branch)
	if err != nil {
		return PublicationReconciliationResult{}, err
	}
	if actual == expected {
		return PublicationReconciliationResult{Outcome: PublicationReconciliationAbsent, Head: actual}, nil
	}
	if actual == "" {
		return PublicationReconciliationResult{Outcome: PublicationReconciliationUnknown}, nil
	}
	if err := lifecycle.git(ctx, "fetch reconciliation branch head", paths.Publication, input.Credential,
		"fetch", "--force", "--no-tags", "origin", actual); err != nil {
		return PublicationReconciliationResult{}, err
	}
	if input.ProposedRevision != "" {
		if err := lifecycle.git(ctx, "verify proposed publication is reachable", paths.Publication, "",
			"merge-base", "--is-ancestor", input.ProposedRevision, actual); err != nil {
			return PublicationReconciliationResult{Outcome: PublicationReconciliationUnknown, Head: actual}, nil
		}
		if err := lifecycle.verifyFirstParentBase(ctx, paths.Publication, base, input.ProposedRevision); err != nil {
			return PublicationReconciliationResult{Outcome: PublicationReconciliationUnknown, Head: actual}, nil
		}
		return PublicationReconciliationResult{Outcome: PublicationReconciliationFound, Head: input.ProposedRevision}, nil
	}
	if input.HistoryPublication {
		return PublicationReconciliationResult{Outcome: PublicationReconciliationUnknown, Head: actual}, nil
	}
	targetTrailer := "Omnigrex-Operation-ID: " + input.OperationID
	output, err := lifecycle.gitOutput(ctx, "find publication operation", paths.Publication, "", nil,
		"log", "--format=%H", "--fixed-strings", "--grep="+targetTrailer, actual)
	if err != nil {
		return PublicationReconciliationResult{}, err
	}
	candidates := strings.Fields(output)
	if len(candidates) != 1 {
		return PublicationReconciliationResult{Outcome: PublicationReconciliationUnknown, Head: actual}, nil
	}
	message, err := lifecycle.gitOutput(ctx, "read publication operation commit", paths.Publication, "", nil,
		"show", "-s", "--format=%B", candidates[0])
	if err != nil {
		return PublicationReconciliationResult{}, err
	}
	parents, err := lifecycle.gitOutput(ctx, "read publication operation parent", paths.Publication, "", nil,
		"rev-list", "--parents", "-n", "1", candidates[0])
	if err != nil {
		return PublicationReconciliationResult{}, err
	}
	parentFields := strings.Fields(parents)
	if !hasExactTrailer(message, targetTrailer) || len(parentFields) != 2 || parentFields[0] != candidates[0] || parentFields[1] != base {
		return PublicationReconciliationResult{Outcome: PublicationReconciliationUnknown, Head: actual}, nil
	}
	return PublicationReconciliationResult{Outcome: PublicationReconciliationFound, Head: candidates[0]}, nil
}

func (lifecycle *Lifecycle) prepareReconciliation(ctx context.Context, destination string, input PublicationReconciliation) (err error) {
	if err := ensureAssignmentDirectory(lifecycle.publicationRoot, filepath.Dir(destination)); err != nil {
		return fmt.Errorf("create publication reconciliation parent: %w", err)
	}
	if _, err := inspectOwnedDirectory(destination); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return fmt.Errorf("replace publication reconciliation checkout: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	if err = lifecycle.git(ctx, "clone publication reconciliation repository", "", input.Credential,
		"clone", "--no-checkout", "--origin=origin", "--config", "core.hooksPath=/dev/null", "--", input.RepositoryURL, destination); err != nil {
		return err
	}
	if _, err = inspectOwnedDirectory(destination); err != nil {
		return err
	}
	if err = lifecycle.git(ctx, "set publication reconciliation remote", destination, "",
		"remote", "set-url", "origin", input.RepositoryURL); err != nil {
		return err
	}
	return lifecycle.git(ctx, "disable publication reconciliation hooks", destination, "",
		"config", "--local", "core.hooksPath", os.DevNull)
}

// ObserveRemoteBranch reads a feature ref before a new Turn is given publication
// authority. It does not accept an agent-controlled remote or retain credentials.
func (lifecycle *Lifecycle) ObserveRemoteBranch(ctx context.Context, repositoryURL, credential, branch string) (string, error) {
	if err := validateRemote(repositoryURL); err != nil {
		return "", err
	}
	if err := validateCredential(credential); err != nil {
		return "", err
	}
	if !validBranch(branch) {
		return "", ErrInvalidPublicationReconciliation
	}
	branchRef := "refs/heads/" + branch
	output, err := lifecycle.gitOutput(ctx, "observe publication branch", "", credential, nil,
		"ls-remote", "--refs", repositoryURL, branchRef)
	if err != nil {
		return "", err
	}
	if output == "" {
		return "", nil
	}
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != branchRef {
		return "", ErrInvalidPublicationReconciliation
	}
	return normalizeObjectID(fields[0])
}

// PrepareRecoveredPublication restores the trusted comparison tree for a
// previously published head. A later Developer handoff must still compare it
// to the agent workspace before becoming a Change Proposal.
func (lifecycle *Lifecycle) PrepareRecoveredPublication(ctx context.Context, assignmentID, repositoryURL, credential, head string) error {
	if err := validateRemote(repositoryURL); err != nil {
		return err
	}
	if err := validateCredential(credential); err != nil {
		return err
	}
	revision, err := normalizeObjectID(head)
	if err != nil {
		return err
	}
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		return err
	}
	release := lifecycle.acquirePublicationLock(assignmentID)
	defer release()
	return lifecycle.preparePublication(ctx, paths.Publication, Publication{RepositoryURL: repositoryURL, Credential: credential}, revision)
}

func (lifecycle *Lifecycle) remoteBranchHead(ctx context.Context, directory, credential, branch string) (string, error) {
	branchRef := "refs/heads/" + branch
	output, err := lifecycle.gitOutput(ctx, "read reconciliation branch head", directory, credential, nil,
		"ls-remote", "--refs", "origin", branchRef)
	if err != nil {
		return "", err
	}
	if output == "" {
		return "", nil
	}
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != branchRef {
		return "", fmt.Errorf("read reconciliation branch head: invalid response")
	}
	head, err := normalizeObjectID(fields[0])
	if err != nil {
		return "", fmt.Errorf("read reconciliation branch head: invalid object ID")
	}
	return head, nil
}

func validatePublicationReconciliation(input PublicationReconciliation) (string, string, error) {
	if err := validateRemote(input.RepositoryURL); err != nil {
		return "", "", err
	}
	if err := validateCredential(input.Credential); err != nil {
		return "", "", err
	}
	base, err := normalizeObjectID(input.BaseRevision)
	if err != nil {
		return "", "", err
	}
	if input.ProposedRevision != "" {
		if _, err := normalizeObjectID(input.ProposedRevision); err != nil || input.ProposedRevision == base || !input.HistoryPublication {
			return "", "", ErrInvalidPublicationReconciliation
		}
	}
	expected := ""
	if input.ExpectedOldHead != "" {
		expected, err = normalizeObjectID(input.ExpectedOldHead)
		if err != nil {
			return "", "", err
		}
		if expected != base {
			return "", "", fmt.Errorf("%w: expected old head must equal publication base", ErrInvalidPublicationReconciliation)
		}
	}
	if !validBranch(input.Branch) || !validOperationID(input.OperationID) {
		return "", "", ErrInvalidPublicationReconciliation
	}
	return base, expected, nil
}

func validOperationID(value string) bool {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func hasExactTrailer(message, target string) bool {
	lines := strings.Split(message, "\n")
	if len(lines) == 0 || lines[len(lines)-1] != target {
		return false
	}
	trailerStart := -1
	for index := len(lines) - 1; index >= 0; index-- {
		if lines[index] == "" {
			trailerStart = index + 1
			break
		}
	}
	if trailerStart <= 0 {
		return false
	}
	for _, line := range lines[trailerStart:] {
		separator := strings.Index(line, ": ")
		if separator <= 0 || separator+2 == len(line) {
			return false
		}
		for _, character := range line[:separator] {
			if character != '-' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
				return false
			}
		}
	}
	return true
}
