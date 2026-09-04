package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxRepositoryFileSize = 256 << 10

var (
	ErrInvalidCommitSHA      = errors.New("invalid Git commit SHA")
	ErrInvalidRepositoryPath = errors.New("invalid repository file path")
)

// DefaultBranch identifies the repository's current default branch and exact head commit.
type DefaultBranch struct {
	Name      string
	CommitSHA string
}

func (client *APIClient) ResolveDefaultBranchCommit(ctx context.Context, installationToken, owner, repository string) (string, error) {
	branch, err := client.ResolveDefaultBranch(ctx, installationToken, owner, repository)
	return branch.CommitSHA, err
}

func (client *APIClient) ResolveDefaultBranch(ctx context.Context, installationToken, owner, repository string) (DefaultBranch, error) {
	if err := validateRepository(owner, repository); err != nil {
		return DefaultBranch{}, err
	}
	var metadata struct {
		DefaultBranch string `json:"default_branch"`
	}
	repositoryPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository)
	if err := client.doJSON(ctx, http.MethodGet, repositoryPath, installationToken, nil, &metadata); err != nil {
		return DefaultBranch{}, err
	}
	if !validGitRefName(metadata.DefaultBranch) {
		return DefaultBranch{}, fmt.Errorf("%w: repository default branch is invalid", ErrInvalidAPIResponse)
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	commitPath := repositoryPath + "/commits/" + url.PathEscape(metadata.DefaultBranch)
	if err := client.doJSON(ctx, http.MethodGet, commitPath, installationToken, nil, &commit); err != nil {
		return DefaultBranch{}, err
	}
	if !validCommitSHA(commit.SHA) {
		return DefaultBranch{}, fmt.Errorf("%w: default branch commit SHA is invalid", ErrInvalidAPIResponse)
	}
	return DefaultBranch{Name: metadata.DefaultBranch, CommitSHA: commit.SHA}, nil
}

func (client *APIClient) FetchRepositoryFile(ctx context.Context, installationToken, owner, repository, filePath, commitSHA string) ([]byte, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if !validRepositoryPath(filePath) {
		return nil, &ConfigurationError{Cause: ErrInvalidRepositoryPath}
	}
	if !validCommitSHA(commitSHA) {
		return nil, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}

	var content struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Size     *int64 `json:"size"`
		Content  string `json:"content"`
	}
	requestPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/contents/" + escapeRepositoryPath(filePath)
	requestPath += "?" + url.Values{"ref": []string{commitSHA}}.Encode()
	if err := client.doJSON(ctx, http.MethodGet, requestPath, installationToken, nil, &content); err != nil {
		return nil, err
	}
	if content.Type != "file" {
		return nil, fmt.Errorf("%w: Contents response type is not file", ErrInvalidAPIResponse)
	}
	if content.Path != filePath {
		return nil, fmt.Errorf("%w: Contents response path does not match request", ErrInvalidAPIResponse)
	}
	if content.Encoding != "base64" {
		return nil, fmt.Errorf("%w: Contents response encoding is not base64", ErrInvalidAPIResponse)
	}
	if content.Size == nil || *content.Size < 0 || *content.Size > MaxRepositoryFileSize {
		return nil, fmt.Errorf("%w: Contents response size is invalid", ErrInvalidAPIResponse)
	}

	maxEncoded := base64.StdEncoding.EncodedLen(MaxRepositoryFileSize)
	if len(content.Content) > maxEncoded+maxEncoded/30+4 {
		return nil, fmt.Errorf("%w: encoded repository file exceeds size limit", ErrInvalidAPIResponse)
	}
	compact := make([]byte, 0, len(content.Content))
	for index := 0; index < len(content.Content); index++ {
		character := content.Content[index]
		if character == '\r' || character == '\n' {
			continue
		}
		compact = append(compact, character)
	}
	if len(compact) > maxEncoded {
		return nil, fmt.Errorf("%w: encoded repository file exceeds size limit", ErrInvalidAPIResponse)
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(compact)))
	decodedLength, err := base64.StdEncoding.Strict().Decode(decoded, compact)
	if err != nil {
		return nil, fmt.Errorf("%w: Contents response has invalid base64: %v", ErrInvalidAPIResponse, err)
	}
	decoded = decoded[:decodedLength]
	if int64(decodedLength) != *content.Size || decodedLength > MaxRepositoryFileSize {
		return nil, fmt.Errorf("%w: decoded repository file size does not match response", ErrInvalidAPIResponse)
	}
	return decoded, nil
}

func validCommitSHA(value string) bool {
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

func validRepositoryPath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if unicode.IsControl(character) {
				return false
			}
		}
	}
	return true
}

func escapeRepositoryPath(value string) string {
	segments := strings.Split(value, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return strings.Join(segments, "/")
}

func validGitRefName(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || value == "@" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.Contains(value, "//") || strings.Contains(value, "\\") {
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
