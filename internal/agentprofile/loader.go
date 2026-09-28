package agentprofile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/turnconfig"
)

var (
	ErrInvalidCommitSHA = errors.New("invalid Agent Profile commit SHA")
	ErrMissingSource    = errors.New("Agent Profile Source is required")
)

type Source interface {
	ResolveDefaultBranchCommit(context.Context, string, string, string) (string, error)
	ListRepositoryDirectoryFiles(context.Context, string, string, string, string, string) ([]string, error)
	FetchRepositoryFile(context.Context, string, string, string, string, string) ([]byte, error)
}

type Loader struct {
	source   Source
	policies role.PolicyCatalog
	allowed  []string
}

func NewLoader(source Source, policies role.PolicyCatalog, allowed ...string) *Loader {
	return &Loader{source: source, policies: policies, allowed: append([]string(nil), allowed...)}
}

type Snapshot struct {
	commitSHA         string
	catalog           Catalog
	policies          role.PolicyCatalog
	profiles          map[Name]Profile
	turnConfiguration turnconfig.Configuration
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
	var turnConfiguration turnconfig.Configuration
	rootFiles, err := loader.source.ListRepositoryDirectoryFiles(ctx, credential, owner, repository, ".omnigrex", commitSHA)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list turn configuration directory: %w", err)
	}
	for _, file := range rootFiles {
		if file == turnconfig.Path {
			content, err := loader.source.FetchRepositoryFile(ctx, credential, owner, repository, turnconfig.Path, commitSHA)
			if err != nil {
				return Snapshot{}, fmt.Errorf("fetch turn configuration: %w", err)
			}
			turnConfiguration, err = turnconfig.Parse(content, loader.allowed)
			if err != nil {
				return Snapshot{}, err
			}
		}
	}

	paths, err := loader.source.ListRepositoryDirectoryFiles(ctx, credential, owner, repository, ".omnigrex/team", commitSHA)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list Agent Profile directory: %w", err)
	}
	sort.Strings(paths)
	profiles := make(map[Name]Profile)
	identities := make([]Identity, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		content, err := loader.source.FetchRepositoryFile(ctx, credential, owner, repository, path, commitSHA)
		if err != nil {
			return Snapshot{}, fmt.Errorf("fetch Agent Profile %s: %w", path, err)
		}
		profile, err := Parse(path, content, loader.policies)
		if err != nil {
			return Snapshot{}, fmt.Errorf("parse Agent Profile %s: %w", path, err)
		}
		if _, duplicate := profiles[profile.Name()]; duplicate {
			return Snapshot{}, fmt.Errorf("%w: duplicate name %q", ErrInvalidCatalog, profile.Name())
		}
		profiles[profile.Name()] = profile
		identities = append(identities, Identity{Name: profile.Name(), Role: profile.Role(), Path: profile.Path()})
	}
	catalog, err := NewCatalog(identities)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{commitSHA: commitSHA, catalog: catalog, policies: loader.policies, profiles: profiles, turnConfiguration: turnConfiguration}, nil
}

func (snapshot Snapshot) CommitSHA() string { return snapshot.commitSHA }
func (snapshot Snapshot) TurnConfiguration() turnconfig.Configuration {
	return turnconfig.Configuration{
		Directories: maps.Clone(snapshot.turnConfiguration.Directories),
		Environment: maps.Clone(snapshot.turnConfiguration.Environment),
	}
}

func (snapshot Snapshot) Profile(name Name) (Profile, bool) {
	profile, ok := snapshot.profiles[name]
	return profile, ok
}

// Select applies a selection policy to the complete discovered catalog.
func (snapshot Snapshot) Select(selector Selector) (Selection, error) {
	if selector == nil {
		return Selection{}, ErrInvalidSelection
	}
	return selector.Select(snapshot.catalog, snapshot.policies)
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
