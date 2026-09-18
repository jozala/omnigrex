package agentprofile

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrInvalidCommitSHA = errors.New("invalid Agent Profile commit SHA")
	ErrMissingSource    = errors.New("Agent Profile Source is required")
)

type Source interface {
	ResolveDefaultBranchCommit(context.Context, string, string, string) (string, error)
	FetchRepositoryFile(context.Context, string, string, string, string, string) ([]byte, error)
}

type Loader struct {
	source    Source
	catalog   Catalog
	selection Selection
}

func NewLoader(source Source) *Loader {
	return &Loader{source: source, catalog: BuiltinCatalog(), selection: BuiltinSelection()}
}

func NewConfiguredLoader(source Source, catalog Catalog, selection Selection) (*Loader, error) {
	if len(catalog.entries) == 0 || len(selection.roles) == 0 {
		return nil, ErrInvalidSelection
	}
	for _, roleID := range selection.roles {
		name, ok := selection.Profile(roleID)
		identity, exists := catalog.Identity(name)
		if !ok || !exists || identity.Role != roleID {
			return nil, ErrInvalidSelection
		}
	}
	return &Loader{source: source, catalog: catalog, selection: selection}, nil
}

type Snapshot struct {
	commitSHA string
	profiles  map[Name]Profile
	byRole    map[Role]Name
}

func (loader *Loader) Load(ctx context.Context, credential, owner, repository string) (Snapshot, error) {
	if loader == nil || loader.source == nil {
		return Snapshot{}, ErrMissingSource
	}
	commitSHA, err := loader.source.ResolveDefaultBranchCommit(ctx, credential, owner, repository)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve Agent Profile default-branch commit: %w", err)
	}
	if !validObjectID(commitSHA) {
		return Snapshot{}, ErrInvalidCommitSHA
	}

	snapshot := Snapshot{commitSHA: commitSHA, profiles: make(map[Name]Profile, len(loader.selection.roles)), byRole: make(map[Role]Name, len(loader.selection.roles))}
	for _, roleID := range loader.selection.roles {
		name, _ := loader.selection.Profile(roleID)
		profile, err := loader.loadProfile(ctx, credential, owner, repository, name, commitSHA)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.profiles[name] = profile
		snapshot.byRole[roleID] = name
	}
	return snapshot, nil
}

func (loader *Loader) loadProfile(ctx context.Context, credential, owner, repository string, name Name, commitSHA string) (Profile, error) {
	identity, ok := loader.catalog.Identity(name)
	if !ok {
		return Profile{}, fmt.Errorf("%w: %q", ErrUnknownProfile, name)
	}
	content, err := loader.source.FetchRepositoryFile(ctx, credential, owner, repository, identity.Path, commitSHA)
	if err != nil {
		return Profile{}, fmt.Errorf("fetch %s Agent Profile: %w", name, err)
	}
	policy, ok := loader.catalog.policy(name)
	if !ok || policy.Role != identity.Role {
		return Profile{}, fmt.Errorf("%w: missing Role policy for %q", ErrInvalidProfile, name)
	}
	profile, err := parse(identity, policy, content)
	if err != nil {
		return Profile{}, fmt.Errorf("parse %s Agent Profile: %w", name, err)
	}
	return profile, nil
}

func (snapshot Snapshot) CommitSHA() string { return snapshot.commitSHA }
func (snapshot Snapshot) Developer() Profile {
	profile, _ := snapshot.ForRole(RoleDeveloper)
	return profile
}
func (snapshot Snapshot) Reviewer() Profile {
	profile, _ := snapshot.ForRole(RoleReviewer)
	return profile
}

func (snapshot Snapshot) Profile(name Name) (Profile, bool) {
	profile, ok := snapshot.profiles[name]
	return profile, ok
}

func (snapshot Snapshot) ForRole(roleID Role) (Profile, bool) {
	name, ok := snapshot.byRole[roleID]
	if !ok {
		return Profile{}, false
	}
	return snapshot.Profile(name)
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return false
			}
		}
	}
	return true
}
