package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CanDecideWorkItem checks current repository permission for the exact actor,
// rather than treating Issue authorship or an event's author_association as authority.
func (client *APIClient) CanDecideWorkItem(ctx context.Context, credential, owner, repository, login string, actorID int64) (bool, error) {
	if err := validateRepository(owner, repository); err != nil {
		return false, err
	}
	if actorID <= 0 || strings.TrimSpace(login) == "" || strings.ContainsAny(login, "/\\\x00") {
		return false, ErrInvalidAPIResponse
	}
	path := fmt.Sprintf("/repos/%s/%s/collaborators/%s/permission", url.PathEscape(owner), url.PathEscape(repository), url.PathEscape(login))
	var response struct {
		Permission string `json:"permission"`
		User       struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, credential, nil, &response); err != nil {
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	if response.User.ID != actorID || !strings.EqualFold(response.User.Login, login) {
		return false, ErrInvalidAPIResponse
	}
	return response.Permission == "write" || response.Permission == "maintain" || response.Permission == "admin", nil
}
