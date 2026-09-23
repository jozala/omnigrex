package gitremote

import (
	"errors"
	"net/url"
	"path"
	"strings"
	"unicode"
)

const DefaultBaseURL = "https://github.com"

var ErrInvalidBaseURL = errors.New("invalid Git remote base URL")

// BaseURL is a validated, credential-free HTTPS base for repository remotes.
type BaseURL struct {
	url url.URL
}

// ParseBaseURL validates and normalizes a Git remote base URL. An empty value selects GitHub.com.
func ParseBaseURL(value string) (BaseURL, error) {
	if value == "" {
		value = DefaultBaseURL
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return BaseURL{}, ErrInvalidBaseURL
	}
	basePath := strings.TrimSuffix(parsed.Path, "/")
	if parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" ||
		strings.ContainsAny(value, "\\#") || containsUnsafePathCharacter(parsed.Path) ||
		basePath != "" && path.Clean(basePath) != basePath {
		return BaseURL{}, ErrInvalidBaseURL
	}
	parsed.Path = basePath
	parsed.RawPath = ""
	return BaseURL{url: *parsed}, nil
}

func containsUnsafePathCharacter(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return true
		}
	}
	return false
}

func (base BaseURL) String() string {
	return base.url.String()
}

// RepositoryURL safely appends an owner and repository as path segments.
func (base BaseURL) RepositoryURL(owner, repository string) (string, error) {
	if base.url.Scheme != "https" || base.url.Hostname() == "" || !validRepositorySegment(owner) || !validRepositorySegment(repository) {
		return "", ErrInvalidBaseURL
	}
	result := base.url
	result.Path += "/" + owner + "/" + repository + ".git"
	return result.String(), nil
}

func validRepositorySegment(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}
