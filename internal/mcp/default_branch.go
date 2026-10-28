package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/gitdelivery"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
)

type SnapshotAcquirer interface {
	AcquireSnapshot(context.Context, string, string, string, string) (gitdelivery.Snapshot, error)
}
type defaultBranchResolver interface {
	GetRepository(context.Context, string, string, string) (githubapi.InstallationRepository, error)
	ResolveDefaultBranch(context.Context, string, string, string) (githubapi.DefaultBranch, error)
}

type defaultBranchResult struct {
	Reason         string          `json:"reason,omitempty"`
	Status         string          `json:"status"`
	Repository     RepositoryScope `json:"repository"`
	Branch         string          `json:"branch"`
	SHA            string          `json:"sha,omitempty"`
	Ref            string          `json:"ref,omitempty"`
	ObservedAt     time.Time       `json:"observed_at"`
	ReobservedAt   *time.Time      `json:"reobserved_at,omitempty"`
	ObservedBranch string          `json:"observed_branch,omitempty"`
	ObservedSHA    string          `json:"observed_sha,omitempty"`
}

func (backend *ProductionBackend) refreshDefaultBranch(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	resolver, ok := backend.github.(defaultBranchResolver)
	if !ok || backend.snapshots == nil || backend.installer == nil || invocation.installFence == nil {
		return nil, ErrToolDependency
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	scope := invocation.Scope
	result := defaultBranchResult{Status: "unavailable", Repository: scope.Repository, Branch: scope.DefaultBranch, ObservedAt: time.Now().UTC()}
	finish := func() (json.RawMessage, error) { return json.Marshal(result) }
	credential, err := backend.credential(ctx, invocation.Name, scope.Role, scope.Repository)
	if err != nil {
		result.Reason = "credential_unavailable"
		return finish()
	}
	repository, err := resolver.GetRepository(ctx, credential, scope.Repository.Owner, scope.Repository.Name)
	if err != nil {
		result.Reason = "repository_unavailable"
		return finish()
	}
	if repository.ID != scope.Repository.ID || !strings.EqualFold(repository.Owner, scope.Repository.Owner) || !strings.EqualFold(repository.Name, scope.Repository.Name) {
		result.Reason = "repository_changed"
		return finish()
	}
	first, err := resolver.ResolveDefaultBranch(ctx, credential, scope.Repository.Owner, scope.Repository.Name)
	result.ObservedAt = time.Now().UTC()
	if err != nil || !validRevision(first.CommitSHA) || !validBranchResourceName(first.Name) {
		result.Reason = "observation_unavailable"
		return finish()
	}
	result.ObservedBranch = first.Name
	result.ObservedSHA = first.CommitSHA
	if first.Name != scope.DefaultBranch {
		result.Status = "default_branch_changed"
		return finish()
	}
	remote, err := backend.remoteBase.RepositoryURL(scope.Repository.Owner, scope.Repository.Name)
	if err != nil {
		return finish()
	}
	// The gateway's durable mutation UUID and exact execution identity make this
	// namespace independent of agent arguments and of existing workspace refs.
	identity := sha256.Sum256([]byte(invocation.Mutation.ExternalResourceID + ":" + invocation.OperationID))
	ref := "refs/omnigrex/default/" + hex.EncodeToString(identity[:])
	snapshot, err := backend.snapshots.AcquireSnapshot(ctx, remote, credential, first.CommitSHA, ref)
	if err != nil {
		result.Reason = "acquisition_unavailable"
		return finish()
	}
	if snapshot.SHA != first.CommitSHA || snapshot.Ref != ref {
		return nil, ErrToolPrecondition
	}
	key := snapshotExecutionKey(scope)
	installCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	err = backend.installer.InstallSnapshot(installCtx, execution.WorkspaceRef{Key: key}, snapshot, invocation.installFence)
	// Installation/resumption errors may leave additive bytes behind. They never
	// become usable success evidence, even if the reference happens to exist.
	if err != nil {
		if errors.Is(err, store.ErrAgentTurnFenceLost) || errors.Is(err, execution.ErrUnknownEffect) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, OutcomeUnknown(err)
		}
		result.Reason = "workspace_rejected"
		return finish()
	}
	result.SHA = snapshot.SHA
	result.Ref = snapshot.Ref
	second, err := resolver.ResolveDefaultBranch(ctx, credential, scope.Repository.Owner, scope.Repository.Name)
	now := time.Now().UTC()
	result.ReobservedAt = &now
	if err != nil || !validRevision(second.CommitSHA) || !validBranchResourceName(second.Name) {
		result.Reason = "reobservation_unavailable"
		return finish()
	}
	repository, err = resolver.GetRepository(ctx, credential, scope.Repository.Owner, scope.Repository.Name)
	if err != nil || repository.ID != scope.Repository.ID || !strings.EqualFold(repository.Owner, scope.Repository.Owner) || !strings.EqualFold(repository.Name, scope.Repository.Name) {
		result.Reason = "repository_changed"
		return finish()
	}
	result.ObservedBranch = second.Name
	result.ObservedSHA = second.CommitSHA
	switch {
	case second.Name != scope.DefaultBranch:
		result.Status = "default_branch_changed"
	case second.CommitSHA != first.CommitSHA:
		result.Status = "stale"
	default:
		result.Status = "current"
	}
	return finish()
}

func snapshotExecutionKey(scope ToolScope) execution.Key {
	return execution.Key{AssignmentID: scope.AgentParticipantID, AgentSessionID: scope.AgentSessionID, AgentTurnID: scope.AgentTurnID, ExecutionEpoch: scope.ExecutionEpoch, RuntimeProfileName: scope.RuntimeProfileName, RuntimeProfileVersion: scope.RuntimeProfileVersion}
}
