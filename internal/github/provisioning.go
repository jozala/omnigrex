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

type repositoryIdentityResponse struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// GetRepository returns the canonical repository identity for owner/name using an installation token.
func (client *APIClient) GetRepository(ctx context.Context, installationToken, owner, repository string) (InstallationRepository, error) {
	if err := validateRepository(owner, repository); err != nil {
		return InstallationRepository{}, err
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository)
	var response repositoryIdentityResponse
	if err := client.doJSON(ctx, http.MethodGet, path, installationToken, nil, &response); err != nil {
		return InstallationRepository{}, err
	}
	observed, err := parseRepositoryIdentity(response)
	if err != nil {
		return InstallationRepository{}, err
	}
	if !strings.EqualFold(observed.Owner, owner) || !strings.EqualFold(observed.Name, repository) {
		return InstallationRepository{}, fmt.Errorf("%w: repository response does not match request", ErrInvalidAPIResponse)
	}
	return observed, nil
}

// GetRepositoryByID resolves a repository's current name using one installation-token request.
func (client *APIClient) GetRepositoryByID(ctx context.Context, installationToken string, repositoryID int64) (InstallationRepository, error) {
	if repositoryID <= 0 {
		return InstallationRepository{}, &ConfigurationError{Cause: ErrInvalidRepositoryID}
	}
	path := fmt.Sprintf("/repositories/%d", repositoryID)
	var response repositoryIdentityResponse
	if err := client.doJSON(ctx, http.MethodGet, path, installationToken, nil, &response); err != nil {
		return InstallationRepository{}, err
	}
	observed, err := parseRepositoryIdentity(response)
	if err != nil {
		return InstallationRepository{}, err
	}
	if observed.ID != repositoryID {
		return InstallationRepository{}, fmt.Errorf("%w: repository ID does not match request", ErrInvalidAPIResponse)
	}
	return observed, nil
}

func parseRepositoryIdentity(response repositoryIdentityResponse) (InstallationRepository, error) {
	if response.ID <= 0 || strings.TrimSpace(response.Name) == "" || strings.TrimSpace(response.FullName) == "" ||
		strings.TrimSpace(response.Owner.Login) == "" {
		return InstallationRepository{}, fmt.Errorf("%w: repository response is incomplete", ErrInvalidAPIResponse)
	}
	ownerFromFull, nameFromFull, found := splitInstallationFullName(response.FullName)
	if !found || !strings.EqualFold(ownerFromFull, response.Owner.Login) || !strings.EqualFold(nameFromFull, response.Name) {
		return InstallationRepository{}, fmt.Errorf("%w: repository full name does not match owner and name", ErrInvalidAPIResponse)
	}
	return InstallationRepository{ID: response.ID, Owner: response.Owner.Login, Name: response.Name}, nil
}

// ListInstallationRepositories returns every repository accessible to an
// installation token, partitioning API-listed entries into valid repositories
// and invalid descriptions. Valid entries are always preserved so one
// malformed entry cannot discard an installation's provisioning work, while
// structural pagination failures (counts, duplicates, page shape) remain hard
// errors because then nothing in the listing can be trusted.
//
// Pagination is bounded at 100 pages of 100 repositories. Installations with
// more than 10,000 accessible repositories fail with ErrInvalidAPIResponse so
// the provisioning delivery is retried to exhaustion and then recorded as an
// observable terminal failure for operator follow-up.
func (client *APIClient) ListInstallationRepositories(ctx context.Context, installationToken string) ([]InstallationRepository, []string, error) {
	repositories := make([]InstallationRepository, 0)
	invalid := make([]string, 0)
	seenIDs := make(map[int64]struct{})
	seenNames := make(map[string]struct{})
	totalCount := -1
	seen := 0
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
			return nil, nil, err
		}
		if payload.TotalCount == nil || *payload.TotalCount < 0 {
			return nil, nil, fmt.Errorf("%w: installation repositories count is invalid", ErrInvalidAPIResponse)
		}
		if totalCount < 0 {
			totalCount = *payload.TotalCount
		} else if *payload.TotalCount != totalCount {
			return nil, nil, fmt.Errorf("%w: installation repositories count changed during pagination", ErrInvalidAPIResponse)
		}
		if payload.Repositories == nil || len(payload.Repositories) > 100 {
			return nil, nil, fmt.Errorf("%w: installation repositories page is invalid", ErrInvalidAPIResponse)
		}
		if len(payload.Repositories) == 0 {
			break
		}
		for _, entry := range payload.Repositories {
			seen++
			if entry.ID <= 0 || strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.FullName) == "" {
				invalid = append(invalid, fmt.Sprintf("repositories[%d] entry is incomplete", seen-1))
				continue
			}
			owner, name, found := splitInstallationFullName(entry.FullName)
			if !found || !strings.EqualFold(name, entry.Name) {
				invalid = append(invalid, fmt.Sprintf("repositories[%d] full name does not match name", seen-1))
				continue
			}
			if err := validateRepository(owner, name); err != nil {
				invalid = append(invalid, fmt.Sprintf("repositories[%d] owner and name are invalid: %v", seen-1, err))
				continue
			}
			if _, exists := seenIDs[entry.ID]; exists {
				return nil, nil, fmt.Errorf("%w: duplicate installation repository id", ErrInvalidAPIResponse)
			}
			key := strings.ToLower(owner) + "/" + strings.ToLower(name)
			if _, exists := seenNames[key]; exists {
				return nil, nil, fmt.Errorf("%w: duplicate installation repository name", ErrInvalidAPIResponse)
			}
			seenIDs[entry.ID] = struct{}{}
			seenNames[key] = struct{}{}
			repositories = append(repositories, InstallationRepository{ID: entry.ID, Owner: owner, Name: name})
		}
		if seen > totalCount {
			return nil, nil, fmt.Errorf("%w: installation repositories exceed total count", ErrInvalidAPIResponse)
		}
		if seen == totalCount {
			break
		}
		// Termination is driven by total_count alone: a short page does not
		// mean the listing is complete, so keep fetching until every
		// accessible repository has been observed.
		if page >= 100 {
			return nil, nil, fmt.Errorf("%w: installation repositories pagination did not converge", ErrInvalidAPIResponse)
		}
	}
	if seen != totalCount {
		return nil, nil, fmt.Errorf("%w: installation repository count does not match paginated results", ErrInvalidAPIResponse)
	}
	return repositories, invalid, nil
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
	ListInstallationRepositories(context.Context, string) ([]InstallationRepository, []string, error)
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

// EnumerateInstallationRepositories returns every repository accessible to
// installationID, partitioning API-listed entries into valid repositories and
// invalid descriptions so valid entries are never discarded with invalid ones.
func (enumerator *InstallationEnumerator) EnumerateInstallationRepositories(ctx context.Context, installationID int64) ([]InstallationRepository, []string, error) {
	if enumerator == nil || enumerator.signer == nil || enumerator.api == nil {
		return nil, nil, errors.New("installation enumerator is not configured")
	}
	if installationID <= 0 {
		return nil, nil, &ConfigurationError{Cause: ErrInvalidInstallationID}
	}
	appJWT, err := enumerator.signer.AppJWT(ctx)
	if err != nil {
		return nil, nil, err
	}
	token, err := enumerator.api.CreateInstallationToken(ctx, appJWT, installationID)
	if err != nil {
		return nil, nil, err
	}
	if token.Token == "" {
		return nil, nil, &ConfigurationError{Cause: ErrInvalidInstallationToken}
	}
	repositories, invalid, err := enumerator.api.ListInstallationRepositories(ctx, token.Token)
	if err != nil {
		return nil, nil, redactSecret(err, appJWT)
	}
	return repositories, invalid, nil
}

func splitInstallationFullName(fullName string) (string, string, bool) {
	owner, name, found := strings.Cut(fullName, "/")
	if !found || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" ||
		strings.Contains(name, "/") || owner != strings.TrimSpace(owner) || name != strings.TrimSpace(name) {
		return "", "", false
	}
	return owner, name, true
}
