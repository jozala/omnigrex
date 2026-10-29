package reviewpolicy

import (
	"context"
	"errors"
)

// Source reads only a caller-selected trusted default-branch revision.
type Source interface {
	ListRepositoryDirectoryFiles(context.Context, string, string, string, string, string) ([]string, error)
	FetchRepositoryFile(context.Context, string, string, string, string, string) ([]byte, error)
}

// LoadAtCommit is shared by admission and read-only preflight. Absence is a
// successful default, while an unreadable directory/file is never absence.
func LoadAtCommit(ctx context.Context, source Source, token, owner, repository, sha string) (Policy, error) {
	if len(sha) != 40 && len(sha) != 64 {
		return Policy{}, errors.New("invalid review policy source revision")
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return Policy{}, errors.New("invalid review policy source revision")
		}
	}
	paths, err := source.ListRepositoryDirectoryFiles(ctx, token, owner, repository, ".omnigrex", sha)
	if err != nil {
		return Policy{}, err
	}
	for _, path := range paths {
		if path == Path {
			content, err := source.FetchRepositoryFile(ctx, token, owner, repository, path, sha)
			if err != nil {
				return Policy{}, err
			}
			return Parse(content)
		}
	}
	return Default(), nil
}
