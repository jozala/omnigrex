package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	APIVersion = "2022-11-28"
	UserAgent  = "omnigrex"
)

var ErrInvalidAPIResponse = errors.New("invalid GitHub API response")

var (
	ErrInvalidIssueNumber       = errors.New("GitHub Issue number must be positive")
	ErrInvalidLabel             = errors.New("invalid GitHub label")
	ErrInvalidPullRequestNumber = errors.New("GitHub Pull Request number must be positive")
	ErrInvalidRepository        = errors.New("GitHub repository owner and name are required")
	ErrInvalidReview            = errors.New("invalid GitHub review")
	ErrInvalidReviewEvent       = errors.New("invalid GitHub review event")
	ErrMissingCredential        = errors.New("GitHub credential is required")
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

type ReviewEvent string

const (
	ReviewApprove        ReviewEvent = "APPROVE"
	ReviewRequestChanges ReviewEvent = "REQUEST_CHANGES"
)

type ReviewRequest struct {
	Body     string      `json:"body"`
	CommitID string      `json:"commit_id"`
	Event    ReviewEvent `json:"event"`
}

type Review struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
}

type API interface {
	ResolveRepositoryInstallation(context.Context, string, string, string) (int64, error)
	VerifyRepositoryInstallation(context.Context, string, int64, string, string) error
	CreateInstallationToken(context.Context, string, int64) (InstallationToken, error)
	ListRepositoryLabels(context.Context, string, string, string) ([]Label, error)
	ListIssueLabels(context.Context, string, string, string, int) ([]Label, error)
	CreateRepositoryLabel(context.Context, string, string, string, Label) (Label, error)
	ReplaceIssueLabels(context.Context, string, string, string, int, []string) ([]Label, error)
	AddIssueLabels(context.Context, string, string, string, int, []string) ([]Label, error)
	RemoveIssueLabel(context.Context, string, string, string, int, string) error
	SubmitReview(context.Context, string, string, string, int, ReviewRequest) (Review, error)
	ResolveDefaultBranchCommit(context.Context, string, string, string) (string, error)
	FetchRepositoryFile(context.Context, string, string, string, string, string) ([]byte, error)
}

type APIClient struct {
	httpClient HTTPDoer
	baseURL    string
}

func NewAPIClient(httpClient HTTPDoer, baseURL string) (*APIClient, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || !secureAPIURL(parsed) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, &ConfigurationError{Cause: fmt.Errorf("invalid GitHub API base URL %q", baseURL)}
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &APIClient{httpClient: httpClient, baseURL: strings.TrimRight(baseURL, "/")}, nil
}

func secureAPIURL(parsed *url.URL) bool {
	if parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (client *APIClient) ResolveRepositoryInstallation(ctx context.Context, appJWT, owner, repository string) (int64, error) {
	if err := validateRepository(owner, repository); err != nil {
		return 0, err
	}
	var installation struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/installation"
	if err := client.doJSON(ctx, http.MethodGet, path, appJWT, nil, &installation); err != nil {
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return 0, &NotInstalledError{Owner: owner, Repository: repository, Cause: err}
		}
		return 0, err
	}
	if installation.ID <= 0 {
		return 0, fmt.Errorf("%w: repository installation has invalid id", ErrInvalidAPIResponse)
	}
	return installation.ID, nil
}

func (client *APIClient) VerifyRepositoryInstallation(ctx context.Context, appJWT string, installationID int64, owner, repository string) error {
	if installationID <= 0 {
		return &ConfigurationError{Cause: ErrInvalidInstallationID}
	}
	resolvedID, err := client.ResolveRepositoryInstallation(ctx, appJWT, owner, repository)
	if err != nil {
		return err
	}
	if resolvedID != installationID {
		return &NotInstalledError{Owner: owner, Repository: repository, InstallationID: installationID}
	}
	return nil
}

func (client *APIClient) CreateInstallationToken(ctx context.Context, appJWT string, installationID int64) (InstallationToken, error) {
	if installationID <= 0 {
		return InstallationToken{}, &ConfigurationError{Cause: ErrInvalidInstallationID}
	}
	var token InstallationToken
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := client.doJSON(ctx, http.MethodPost, path, appJWT, struct{}{}, &token); err != nil {
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return InstallationToken{}, &NotInstalledError{InstallationID: installationID, Cause: err}
		}
		return InstallationToken{}, err
	}
	if token.Token == "" || token.ExpiresAt.IsZero() {
		return InstallationToken{}, fmt.Errorf("%w: installation token is incomplete", ErrInvalidAPIResponse)
	}
	return token, nil
}

func (client *APIClient) ListRepositoryLabels(ctx context.Context, installationToken, owner, repository string) ([]Label, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/labels?per_page=100"
	return client.listLabels(ctx, installationToken, path)
}

func (client *APIClient) ListIssueLabels(ctx context.Context, installationToken, owner, repository string, issueNumber int) ([]Label, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if issueNumber <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidIssueNumber}
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels?per_page=100", url.PathEscape(owner), url.PathEscape(repository), issueNumber)
	return client.listLabels(ctx, installationToken, path)
}

func (client *APIClient) CreateRepositoryLabel(ctx context.Context, installationToken, owner, repository string, label Label) (Label, error) {
	if err := validateRepository(owner, repository); err != nil {
		return Label{}, err
	}
	if !validLabelName(label.Name) || len(label.Color) != 6 || !allHexadecimal(label.Color) {
		return Label{}, &ConfigurationError{Cause: ErrInvalidLabel}
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/labels"
	var created Label
	if err := client.doJSON(ctx, http.MethodPost, path, installationToken, label, &created); err != nil {
		return Label{}, err
	}
	return created, nil
}

func (client *APIClient) ReplaceIssueLabels(ctx context.Context, installationToken, owner, repository string, issueNumber int, labels []string) ([]Label, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if issueNumber <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidIssueNumber}
	}
	for _, label := range labels {
		if !validLabelName(label) {
			return nil, &ConfigurationError{Cause: ErrInvalidLabel}
		}
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", url.PathEscape(owner), url.PathEscape(repository), issueNumber)
	body := struct {
		Labels []string `json:"labels"`
	}{Labels: labels}
	var replaced []Label
	if err := client.doJSON(ctx, http.MethodPut, path, installationToken, body, &replaced); err != nil {
		return nil, err
	}
	return replaced, nil
}

func (client *APIClient) AddIssueLabels(ctx context.Context, installationToken, owner, repository string, issueNumber int, labels []string) ([]Label, error) {
	if issueNumber <= 0 {
		return nil, ErrInvalidIssueNumber
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", url.PathEscape(owner), url.PathEscape(repository), issueNumber)
	body := struct {
		Labels []string `json:"labels"`
	}{Labels: labels}
	var added []Label
	if err := client.doJSON(ctx, http.MethodPost, path, installationToken, body, &added); err != nil {
		return nil, err
	}
	return added, nil
}

func (client *APIClient) RemoveIssueLabel(ctx context.Context, installationToken, owner, repository string, issueNumber int, label string) error {
	if issueNumber <= 0 {
		return ErrInvalidIssueNumber
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s", url.PathEscape(owner), url.PathEscape(repository), issueNumber, url.PathEscape(label))
	return client.doJSON(ctx, http.MethodDelete, path, installationToken, nil, nil)
}

func (client *APIClient) SubmitReview(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int, review ReviewRequest) (Review, error) {
	if err := validateRepository(owner, repository); err != nil {
		return Review{}, err
	}
	if pullRequestNumber <= 0 {
		return Review{}, &ConfigurationError{Cause: ErrInvalidPullRequestNumber}
	}
	if review.Event != ReviewApprove && review.Event != ReviewRequestChanges {
		return Review{}, &ConfigurationError{Cause: ErrInvalidReviewEvent}
	}
	if review.Event == ReviewRequestChanges && strings.TrimSpace(review.Body) == "" {
		return Review{}, &ConfigurationError{Cause: ErrInvalidReview}
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	var submitted Review
	if err := client.doJSON(ctx, http.MethodPost, path, installationToken, review, &submitted); err != nil {
		return Review{}, err
	}
	return submitted, nil
}

func (client *APIClient) listLabels(ctx context.Context, installationToken, firstPath string) ([]Label, error) {
	labels := make([]Label, 0)
	path := firstPath
	seen := make(map[string]struct{})
	for path != "" {
		if _, exists := seen[path]; exists {
			return nil, fmt.Errorf("%w: repeated pagination link", ErrInvalidAPIResponse)
		}
		seen[path] = struct{}{}
		var page []Label
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, installationToken, nil, &page)
		if err != nil {
			return nil, err
		}
		labels = append(labels, page...)
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		path, err = client.paginationPath(next)
		if err != nil {
			return nil, err
		}
	}
	return labels, nil
}

func (client *APIClient) doJSON(ctx context.Context, method, path, credential string, body, destination any) error {
	_, err := client.doJSONWithHeaders(ctx, method, path, credential, body, destination)
	return err
}

func (client *APIClient) doJSONWithHeaders(ctx context.Context, method, path, credential string, body, destination any) (http.Header, error) {
	if strings.TrimSpace(credential) == "" {
		return nil, &ConfigurationError{Cause: ErrMissingCredential}
	}
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode GitHub API request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, requestBody)
	if err != nil {
		return nil, fmt.Errorf("create GitHub API request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", APIVersion)
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("Authorization", "Bearer "+credential)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &TransientError{Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, classifyAPIError(response, method, path)
	}
	header := response.Header.Clone()
	if destination == nil || response.StatusCode == http.StatusNoContent {
		return header, nil
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrInvalidAPIResponse, err)
	}
	return header, nil
}

func validateRepository(owner, repository string) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repository) == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") {
		return &ConfigurationError{Cause: ErrInvalidRepository}
	}
	return nil
}

func validLabelName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 50
}

func allHexadecimal(value string) bool {
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

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}
		isNext := false
		for _, attribute := range sections[1:] {
			if strings.TrimSpace(attribute) == `rel="next"` {
				isNext = true
				break
			}
		}
		if isNext {
			return strings.Trim(strings.TrimSpace(sections[0]), "<>")
		}
	}
	return ""
}

func (client *APIClient) paginationPath(next string) (string, error) {
	nextURL, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("%w: invalid pagination link", ErrInvalidAPIResponse)
	}
	baseURL, _ := url.Parse(client.baseURL)
	if !nextURL.IsAbs() {
		if !strings.HasPrefix(next, "/") {
			return "", fmt.Errorf("%w: invalid relative pagination link", ErrInvalidAPIResponse)
		}
		return next, nil
	}
	if !strings.EqualFold(nextURL.Scheme, baseURL.Scheme) || !strings.EqualFold(nextURL.Host, baseURL.Host) {
		return "", fmt.Errorf("%w: cross-origin pagination link", ErrInvalidAPIResponse)
	}
	basePath := strings.TrimRight(baseURL.EscapedPath(), "/")
	if basePath != "" && nextURL.EscapedPath() != basePath && !strings.HasPrefix(nextURL.EscapedPath(), basePath+"/") {
		return "", fmt.Errorf("%w: pagination link is outside API base path", ErrInvalidAPIResponse)
	}
	path := strings.TrimPrefix(nextURL.EscapedPath(), basePath)
	if path == "" {
		path = "/"
	}
	if nextURL.RawQuery != "" {
		path += "?" + nextURL.RawQuery
	}
	return path, nil
}

func classifyAPIError(response *http.Response, method, path string) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Message == "" {
		payload.Message = strings.TrimSpace(string(body))
	}
	apiError := &APIError{
		StatusCode: response.StatusCode,
		Method:     method,
		Path:       path,
		Message:    payload.Message,
		RequestID:  response.Header.Get("X-GitHub-Request-Id"),
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden && isRateLimited(response.Header) {
		return &RateLimitError{
			APIError:   apiError,
			RetryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
			ResetAt:    parseRateLimitReset(response.Header.Get("X-RateLimit-Reset")),
		}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return &PermissionError{APIError: apiError}
	}
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= http.StatusInternalServerError {
		return &TransientError{Cause: apiError}
	}
	return apiError
}

func isRateLimited(header http.Header) bool {
	return header.Get("Retry-After") != "" || header.Get("X-RateLimit-Remaining") == "0"
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return max(time.Until(retryAt), 0)
	}
	return 0
}

func parseRateLimitReset(value string) time.Time {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}
