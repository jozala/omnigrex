package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// InstallationRepository is one repository accessible to a GitHub App installation.
type InstallationRepository struct {
	ID    int64
	Owner string
	Name  string
}

// GetRepository returns the canonical repository identity for owner/name using an installation token.
func (client *APIClient) GetRepository(ctx context.Context, installationToken, owner, repository string) (InstallationRepository, error) {
	if err := validateRepository(owner, repository); err != nil {
		return InstallationRepository{}, err
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository)
	var response struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := client.doJSON(ctx, http.MethodGet, path, installationToken, nil, &response); err != nil {
		return InstallationRepository{}, err
	}
	if response.ID <= 0 || strings.TrimSpace(response.Name) == "" || strings.TrimSpace(response.FullName) == "" ||
		strings.TrimSpace(response.Owner.Login) == "" {
		return InstallationRepository{}, fmt.Errorf("%w: repository response is incomplete", ErrInvalidAPIResponse)
	}
	ownerFromFull, nameFromFull, found := splitInstallationFullName(response.FullName)
	if !found || !strings.EqualFold(ownerFromFull, response.Owner.Login) || !strings.EqualFold(nameFromFull, response.Name) {
		return InstallationRepository{}, fmt.Errorf("%w: repository full name does not match owner and name", ErrInvalidAPIResponse)
	}
	if !strings.EqualFold(response.Owner.Login, owner) || !strings.EqualFold(response.Name, repository) {
		return InstallationRepository{}, fmt.Errorf("%w: repository response does not match request", ErrInvalidAPIResponse)
	}
	return InstallationRepository{ID: response.ID, Owner: response.Owner.Login, Name: response.Name}, nil
}

// ListInstallationRepositories returns every repository accessible to an installation token.
func (client *APIClient) ListInstallationRepositories(ctx context.Context, installationToken string) ([]InstallationRepository, error) {
	repositories := make([]InstallationRepository, 0)
	seenIDs := make(map[int64]struct{})
	seenNames := make(map[string]struct{})
	totalCount := -1
	for page := 1; ; page++ {
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		var payload struct {
			TotalCount   *int `json:"total_count"`
			Repositories []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := client.doJSON(ctx, http.MethodGet, path, installationToken, nil, &payload); err != nil {
			return nil, err
		}
		if payload.TotalCount == nil || *payload.TotalCount < 0 {
			return nil, fmt.Errorf("%w: installation repositories count is invalid", ErrInvalidAPIResponse)
		}
		if totalCount < 0 {
			totalCount = *payload.TotalCount
		} else if *payload.TotalCount != totalCount {
			return nil, fmt.Errorf("%w: installation repositories count changed during pagination", ErrInvalidAPIResponse)
		}
		if payload.Repositories == nil || len(payload.Repositories) > 100 {
			return nil, fmt.Errorf("%w: installation repositories page is invalid", ErrInvalidAPIResponse)
		}
		if len(payload.Repositories) == 0 {
			break
		}
		for _, entry := range payload.Repositories {
			if entry.ID <= 0 || strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.FullName) == "" {
				return nil, fmt.Errorf("%w: installation repository entry is incomplete", ErrInvalidAPIResponse)
			}
			owner, name, found := splitInstallationFullName(entry.FullName)
			if !found || !strings.EqualFold(name, entry.Name) {
				return nil, fmt.Errorf("%w: installation repository full name does not match name", ErrInvalidAPIResponse)
			}
			if err := validateRepository(owner, name); err != nil {
				return nil, fmt.Errorf("%w: installation repository owner and name are invalid: %v", ErrInvalidAPIResponse, err)
			}
			if _, exists := seenIDs[entry.ID]; exists {
				return nil, fmt.Errorf("%w: duplicate installation repository id", ErrInvalidAPIResponse)
			}
			key := strings.ToLower(owner) + "/" + strings.ToLower(name)
			if _, exists := seenNames[key]; exists {
				return nil, fmt.Errorf("%w: duplicate installation repository name", ErrInvalidAPIResponse)
			}
			seenIDs[entry.ID] = struct{}{}
			seenNames[key] = struct{}{}
			repositories = append(repositories, InstallationRepository{ID: entry.ID, Owner: owner, Name: name})
		}
		if len(repositories) > totalCount {
			return nil, fmt.Errorf("%w: installation repositories exceed total count", ErrInvalidAPIResponse)
		}
		if len(repositories) == totalCount {
			break
		}
		if len(payload.Repositories) < 100 {
			break
		}
		if page >= 100 {
			return nil, fmt.Errorf("%w: installation repositories pagination did not converge", ErrInvalidAPIResponse)
		}
	}
	if len(repositories) != totalCount {
		return nil, fmt.Errorf("%w: installation repository count does not match paginated results", ErrInvalidAPIResponse)
	}
	return repositories, nil
}

// InstallationEnumerator lists every repository accessible to one installation
// without trusting the webhook repository list.
type InstallationEnumerator struct {
	signer AppJWTProvider
	api    InstallationRepositoriesAPI
}

// InstallationRepositoriesAPI is the narrow GitHub API surface needed for enumeration.
type InstallationRepositoriesAPI interface {
	CreateInstallationToken(context.Context, string, int64) (InstallationToken, error)
	ListInstallationRepositories(context.Context, string) ([]InstallationRepository, error)
}

// NewInstallationEnumerator creates an enumerator for one GitHub App identity.
func NewInstallationEnumerator(signer AppJWTProvider, api InstallationRepositoriesAPI) (*InstallationEnumerator, error) {
	if signer == nil {
		return nil, errors.New("installation enumerator App JWT provider is nil")
	}
	if api == nil {
		return nil, errors.New("installation enumerator API is nil")
	}
	return &InstallationEnumerator{signer: signer, api: api}, nil
}

// EnumerateInstallationRepositories returns every repository accessible to installationID.
func (enumerator *InstallationEnumerator) EnumerateInstallationRepositories(ctx context.Context, installationID int64) ([]InstallationRepository, error) {
	if enumerator == nil || enumerator.signer == nil || enumerator.api == nil {
		return nil, errors.New("installation enumerator is not configured")
	}
	if installationID <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidInstallationID}
	}
	appJWT, err := enumerator.signer.AppJWT(ctx)
	if err != nil {
		return nil, err
	}
	token, err := enumerator.api.CreateInstallationToken(ctx, appJWT, installationID)
	if err != nil {
		return nil, err
	}
	if token.Token == "" {
		return nil, &ConfigurationError{Cause: ErrInvalidInstallationToken}
	}
	repositories, err := enumerator.api.ListInstallationRepositories(ctx, token.Token)
	if err != nil {
		return nil, redactSecret(err, appJWT)
	}
	return repositories, nil
}

func splitInstallationFullName(fullName string) (string, string, bool) {
	owner, name, found := strings.Cut(fullName, "/")
	if !found || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" ||
		strings.Contains(name, "/") || owner != strings.TrimSpace(owner) || name != strings.TrimSpace(name) {
		return "", "", false
	}
	return owner, name, true
}
