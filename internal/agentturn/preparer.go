// Package agentturn coordinates durable Agent Turn preparation and execution.
package agentturn

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jozala/omnigrex/internal/agentprofile"
	githubapi "github.com/jozala/omnigrex/internal/github"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

var (
	ErrDependencyNil                   = errors.New("Agent Turn Preparer dependency is nil")
	ErrInvalidRequest                  = errors.New("invalid Agent Turn preparation request")
	ErrInvalidRuntimeProfileReference  = errors.New("invalid Runtime Profile reference")
	ErrRuntimeProfileReferenceMismatch = errors.New("resolved Runtime Profile reference does not match Agent Profile")
	ErrAssignmentConfigurationConflict = errors.New("Agent Assignment configuration conflict")
)

// AssignmentConfigurationConflictError carries the credential-free preparation used to detect binding drift.
type AssignmentConfigurationConflictError struct {
	Preparation store.AgentTurnPreparationSpec
	Cause       error
}

func (err *AssignmentConfigurationConflictError) Error() string {
	return fmt.Sprintf("%v: %v", ErrAssignmentConfigurationConflict, err.Cause)
}

func (err *AssignmentConfigurationConflictError) Unwrap() []error {
	return []error{ErrAssignmentConfigurationConflict, err.Cause}
}

// ProfileLoader loads both Role profiles from one repository snapshot.
type ProfileLoader interface {
	Load(context.Context, string, string, string) (agentprofile.Snapshot, error)
}

// RuntimeRegistry resolves one exact immutable Runtime Profile reference.
type RuntimeRegistry interface {
	Resolve(string, string) (runtimeprofile.Profile, error)
}

// PreparationStore commits the complete two-Role preparation under a job fence.
type PreparationStore interface {
	PrepareAgentTurn(context.Context, store.JobLease, store.AgentTurnPreparationSpec) (store.AgentTurnPreparationCommit, error)
}

var (
	_ ProfileLoader    = (*agentprofile.Loader)(nil)
	_ RuntimeRegistry  = runtimeprofile.Registry{}
	_ PreparationStore = (*store.Store)(nil)
)

// Request identifies the live preparation Job and repository profile source.
type Request struct {
	Lease                  store.JobLease
	InstallationCredential string
	RepositoryOwner        string
	RepositoryName         string
}

// Result carries the durable Store commit and the Runtime Profile selected by its fenced Role.
type Result struct {
	Commit         store.AgentTurnPreparationCommit
	RuntimeProfile runtimeprofile.Profile
}

// Preparer translates repository Agent Profiles into a durable preparation specification.
type Preparer struct {
	loader   ProfileLoader
	registry RuntimeRegistry
	store    PreparationStore
}

func NewPreparer(loader ProfileLoader, registry RuntimeRegistry, preparationStore PreparationStore) *Preparer {
	return &Preparer{loader: loader, registry: registry, store: preparationStore}
}

// Prepare resolves both Role configurations and commits one fenced Agent Turn preparation.
func (preparer *Preparer) Prepare(ctx context.Context, request Request) (result Result, err error) {
	defer func() {
		if err != nil {
			err = sanitizePreparationError(err, request.InstallationCredential)
		}
	}()
	if preparer == nil || nilDependency(preparer.loader) || nilDependency(preparer.registry) || nilDependency(preparer.store) {
		return Result{}, ErrDependencyNil
	}
	if strings.TrimSpace(request.InstallationCredential) == "" || strings.TrimSpace(request.RepositoryOwner) == "" ||
		strings.TrimSpace(request.RepositoryName) == "" || request.Lease.Kind != store.PrepareAgentTurnJobKind ||
		request.Lease.Status != store.JobLeased {
		return Result{}, ErrInvalidRequest
	}

	snapshot, err := preparer.loader.Load(ctx, request.InstallationCredential, request.RepositoryOwner, request.RepositoryName)
	if err != nil {
		return Result{}, fmt.Errorf("load Agent Profiles: %w", err)
	}

	developer, developerRuntime, err := preparer.prepareRole(snapshot.Developer(), snapshot.CommitSHA())
	if err != nil {
		return Result{}, fmt.Errorf("prepare Developer profile: %w", err)
	}
	reviewer, reviewerRuntime, err := preparer.prepareRole(snapshot.Reviewer(), snapshot.CommitSHA())
	if err != nil {
		return Result{}, fmt.Errorf("prepare Reviewer profile: %w", err)
	}

	commit, err := preparer.store.PrepareAgentTurn(ctx, request.Lease, store.AgentTurnPreparationSpec{
		Developer: developer,
		Reviewer:  reviewer,
	})
	if err != nil {
		if errors.Is(err, store.ErrAssignmentConfigurationConflict) {
			return Result{}, &AssignmentConfigurationConflictError{
				Preparation: store.AgentTurnPreparationSpec{Developer: developer, Reviewer: reviewer},
				Cause:       err,
			}
		}
		return Result{}, fmt.Errorf("commit Agent Turn preparation: %w", err)
	}
	selectedRuntime := developerRuntime
	if commit.Assignment.Role == workflow.RoleReviewer {
		selectedRuntime = reviewerRuntime
	}
	return Result{Commit: commit, RuntimeProfile: selectedRuntime}, nil
}

func (preparer *Preparer) prepareRole(profile agentprofile.Profile, commitSHA string) (store.RolePreparation, runtimeprofile.Profile, error) {
	name, version, ok := strings.Cut(profile.Runtime(), "/")
	if !ok || name == "" || version == "" || strings.Contains(version, "/") {
		return store.RolePreparation{}, runtimeprofile.Profile{}, ErrInvalidRuntimeProfileReference
	}
	resolved, err := preparer.registry.Resolve(name, version)
	if err != nil {
		return store.RolePreparation{}, runtimeprofile.Profile{}, fmt.Errorf("resolve Runtime Profile %s/%s: %w", name, version, err)
	}
	contract := resolved.Contract()
	if contract.Name != name || contract.Version != version {
		return store.RolePreparation{}, runtimeprofile.Profile{}, fmt.Errorf("%w: requested %s/%s, resolved %s/%s", ErrRuntimeProfileReferenceMismatch, name, version, contract.Name, contract.Version)
	}
	hash := profile.ContentSHA256()
	return store.RolePreparation{
		Binding: store.AssignmentRuntimeBinding{
			AgentProfileName:            string(profile.Name()),
			RuntimeProfileName:          contract.Name,
			RuntimeProfileVersion:       contract.Version,
			RuntimeProfileContentSHA256: resolved.ContentSHA256(),
			RuntimeImageDigest:          contract.Image,
		},
		Profile: store.AgentProfileSnapshot{
			CommitSHA:     commitSHA,
			ContentSHA256: hash[:],
			Config:        profile.CanonicalJSON(),
		},
	}, resolved, nil
}

type credentialSafeError struct {
	message  string
	metadata githubapi.SafeErrorMetadata
}

func (err credentialSafeError) Error() string { return err.message }
func (err credentialSafeError) SafeErrorMetadata() githubapi.SafeErrorMetadata {
	return err.metadata
}

func sanitizePreparationError(err error, credential string) error {
	var conflict *AssignmentConfigurationConflictError
	if errors.As(err, &conflict) {
		return &AssignmentConfigurationConflictError{
			Preparation: conflict.Preparation,
			Cause:       ErrAssignmentConfigurationConflict,
		}
	}
	return redactCredential(err, credential)
}

func redactCredential(err error, credential string) error {
	if credential == "" {
		return err
	}
	message := err.Error()
	message = strings.ReplaceAll(message, credential, "[REDACTED]")
	metadata := githubapi.ExtractSafeErrorMetadata(err)
	if isPermanentWorkerError(err) {
		metadata.Permanent = true
	}
	return credentialSafeError{message: message, metadata: metadata}
}

func nilDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
