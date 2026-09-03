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
	source Source
}

func NewLoader(source Source) *Loader {
	return &Loader{source: source}
}

type Snapshot struct {
	commitSHA string
	developer Profile
	reviewer  Profile
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

	developer, err := loader.loadProfile(ctx, credential, owner, repository, Developer, commitSHA)
	if err != nil {
		return Snapshot{}, err
	}
	reviewer, err := loader.loadProfile(ctx, credential, owner, repository, Reviewer, commitSHA)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{commitSHA: commitSHA, developer: developer, reviewer: reviewer}, nil
}

func (loader *Loader) loadProfile(ctx context.Context, credential, owner, repository string, name Name, commitSHA string) (Profile, error) {
	identity, err := identityFor(name)
	if err != nil {
		return Profile{}, err
	}
	content, err := loader.source.FetchRepositoryFile(ctx, credential, owner, repository, identity.path, commitSHA)
	if err != nil {
		return Profile{}, fmt.Errorf("fetch %s Agent Profile: %w", name, err)
	}
	profile, err := Parse(name, content)
	if err != nil {
		return Profile{}, fmt.Errorf("parse %s Agent Profile: %w", name, err)
	}
	return profile, nil
}

func (snapshot Snapshot) CommitSHA() string  { return snapshot.commitSHA }
func (snapshot Snapshot) Developer() Profile { return snapshot.developer }
func (snapshot Snapshot) Reviewer() Profile  { return snapshot.reviewer }

func (snapshot Snapshot) Profile(name Name) (Profile, bool) {
	switch name {
	case Developer:
		return snapshot.developer, true
	case Reviewer:
		return snapshot.reviewer, true
	default:
		return Profile{}, false
	}
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
