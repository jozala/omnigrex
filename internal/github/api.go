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
	"unicode"
)

const (
	APIVersion = "2022-11-28"
	UserAgent  = "omnigrex"
)

var ErrInvalidAPIResponse = errors.New("invalid GitHub API response")

var (
	ErrInvalidIssueNumber       = errors.New("GitHub Issue number must be positive")
	ErrInvalidComment           = errors.New("invalid GitHub comment")
	ErrInvalidLabel             = errors.New("invalid GitHub label")
	ErrInvalidMarker            = errors.New("invalid GitHub hidden marker")
	ErrInvalidPullRequest       = errors.New("invalid GitHub Pull Request")
	ErrInvalidPullRequestNumber = errors.New("GitHub Pull Request number must be positive")
	ErrInvalidRepository        = errors.New("GitHub repository owner and name are required")
	ErrInvalidRepositoryID      = errors.New("GitHub repository ID must be positive")
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

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type Issue struct {
	ID      int64   `json:"id"`
	NodeID  string  `json:"node_id"`
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	State   string  `json:"state"`
	HTMLURL string  `json:"html_url"`
	User    User    `json:"user"`
	Labels  []Label `json:"labels"`
}

type IssueComment struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PullRequestBranch struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
}

type PullRequest struct {
	ID      int64             `json:"id"`
	NodeID  string            `json:"node_id"`
	Number  int               `json:"number"`
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	State   string            `json:"state"`
	HTMLURL string            `json:"html_url"`
	User    User              `json:"user"`
	Head    PullRequestBranch `json:"head"`
	Base    PullRequestBranch `json:"base"`
	Draft   bool              `json:"draft"`
	Merged  bool              `json:"merged"`
}

type PullRequestFileStatus string

const (
	PullRequestFileAdded     PullRequestFileStatus = "added"
	PullRequestFileRemoved   PullRequestFileStatus = "removed"
	PullRequestFileModified  PullRequestFileStatus = "modified"
	PullRequestFileRenamed   PullRequestFileStatus = "renamed"
	PullRequestFileCopied    PullRequestFileStatus = "copied"
	PullRequestFileChanged   PullRequestFileStatus = "changed"
	PullRequestFileUnchanged PullRequestFileStatus = "unchanged"
)

type PullRequestFile struct {
	Filename string                `json:"filename"`
	Status   PullRequestFileStatus `json:"status"`
	// Patch is nil when GitHub does not provide textual diff information, including for binary files.
	// A nil patch supplies no commentable line locations.
	Patch *string `json:"patch"`
}

type ReviewComment struct {
	ID                  int64     `json:"id"`
	NodeID              string    `json:"node_id"`
	PullRequestReviewID *int64    `json:"pull_request_review_id,omitempty"`
	InReplyToID         int64     `json:"in_reply_to_id"`
	Body                string    `json:"body"`
	Path                string    `json:"path"`
	CommitID            string    `json:"commit_id"`
	OriginalCommitID    string    `json:"original_commit_id"`
	HTMLURL             string    `json:"html_url"`
	PullRequestURL      string    `json:"pull_request_url"`
	User                User      `json:"user"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	Line                *int      `json:"line"`
	StartLine           *int      `json:"start_line"`
	OriginalLine        *int      `json:"original_line"`
	OriginalStartLine   *int      `json:"original_start_line"`
	Position            *int      `json:"position"`
	Side                string    `json:"side"`
	StartSide           string    `json:"start_side"`
	SubjectType         string    `json:"subject_type"`
}

type ReviewThread struct {
	ID       string          `json:"id"`
	Path     string          `json:"path"`
	Resolved bool            `json:"resolved"`
	Outdated bool            `json:"outdated"`
	Comments []ReviewComment `json:"comments"`
}

type CheckRun struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	HTMLURL     string     `json:"html_url"`
	DetailsURL  string     `json:"details_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type OpenPullRequestRequest struct {
	Title       string
	Body        string
	Head        string
	Base        string
	IssueNumber int
	Marker      string
}

// ListPullRequestsRequest selects an exact same-repository head branch and an optional base branch.
type ListPullRequestsRequest struct {
	Head string
	Base string
}

type CommentRequest struct {
	Body   string
	Marker string
}

type ReviewEvent string

type ReviewSide string

const (
	ReviewApprove        ReviewEvent = "APPROVE"
	ReviewRequestChanges ReviewEvent = "REQUEST_CHANGES"
	ReviewSideLeft       ReviewSide  = "LEFT"
	ReviewSideRight      ReviewSide  = "RIGHT"
)

type ReviewRequest struct {
	Body     string                 `json:"body"`
	CommitID string                 `json:"commit_id"`
	Event    ReviewEvent            `json:"event"`
	Comments []ReviewCommentRequest `json:"comments,omitempty"`
}

type ReviewCommentRequest struct {
	Path      string     `json:"path"`
	Body      string     `json:"body"`
	Line      int        `json:"line"`
	Side      ReviewSide `json:"side"`
	StartLine int        `json:"start_line,omitempty"`
	StartSide ReviewSide `json:"start_side,omitempty"`
}

type Review struct {
	ID             int64     `json:"id"`
	NodeID         string    `json:"node_id"`
	State          string    `json:"state"`
	Body           string    `json:"body"`
	CommitID       string    `json:"commit_id"`
	HTMLURL        string    `json:"html_url"`
	PullRequestURL string    `json:"pull_request_url"`
	User           User      `json:"user"`
	SubmittedAt    time.Time `json:"submitted_at"`
}

type API interface {
	ResolveRepositoryInstallation(context.Context, string, string, string) (int64, error)
	VerifyRepositoryInstallation(context.Context, string, int64, string, string) error
	CreateInstallationToken(context.Context, string, int64) (InstallationToken, error)
	GetIssue(context.Context, string, string, string, int) (Issue, error)
	ListIssueComments(context.Context, string, string, string, int) ([]IssueComment, error)
	GetPullRequest(context.Context, string, string, string, int) (PullRequest, error)
	ListPullRequests(context.Context, string, string, string, ListPullRequestsRequest) ([]PullRequest, error)
	ListPullRequestFiles(context.Context, string, string, string, int) ([]PullRequestFile, error)
	ListPullRequestReviews(context.Context, string, string, string, int) ([]Review, error)
	ListReviewThreads(context.Context, string, string, string, int) ([]ReviewThread, error)
	GetCheckRuns(context.Context, string, string, string, string) ([]CheckRun, error)
	OpenPullRequest(context.Context, string, string, string, OpenPullRequestRequest) (PullRequest, error)
	CreateIssueComment(context.Context, string, string, string, int, CommentRequest) (IssueComment, error)
	CreatePullRequestComment(context.Context, string, string, string, int, CommentRequest) (IssueComment, error)
	DeleteIssueComment(context.Context, string, string, string, int64) error
	ListRepositoryLabels(context.Context, string, string, string) ([]Label, error)
	ListIssueLabels(context.Context, string, string, string, int) ([]Label, error)
	CreateRepositoryLabel(context.Context, string, string, string, Label) (Label, error)
	AddIssueLabels(context.Context, string, string, string, int, []string) ([]Label, error)
	RemoveIssueLabel(context.Context, string, string, string, int, string) error
	SubmitReview(context.Context, string, string, string, int, ReviewRequest) (Review, error)
	ResolveDefaultBranch(context.Context, string, string, string) (DefaultBranch, error)
	ResolveDefaultBranchCommit(context.Context, string, string, string) (string, error)
	ListRepositoryDirectoryFiles(context.Context, string, string, string, string, string) ([]string, error)
	FetchRepositoryFile(context.Context, string, string, string, string, string) ([]byte, error)
}

type APIClient struct {
	httpClient HTTPDoer
	baseURL    string
}

type AuthenticatedApp struct {
	ID          int64                   `json:"id"`
	Events      []string                `json:"events"`
	Permissions InstallationPermissions `json:"permissions"`
}

type AppWebhookConfig struct {
	URL         string          `json:"url"`
	ContentType string          `json:"content_type"`
	Secret      string          `json:"secret"`
	InsecureSSL json.RawMessage `json:"insecure_ssl"`
}

func (configuration AppWebhookConfig) VerifiesTLS() bool {
	return strings.TrimSpace(string(configuration.InsecureSSL)) == "0" || strings.TrimSpace(string(configuration.InsecureSSL)) == `"0"`
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

// GetAuthenticatedApp returns the App identity and configured repository event policy for one App JWT.
func (client *APIClient) GetAuthenticatedApp(ctx context.Context, appJWT string) (AuthenticatedApp, error) {
	var app AuthenticatedApp
	if err := client.doJSON(ctx, http.MethodGet, "/app", appJWT, nil, &app); err != nil {
		return AuthenticatedApp{}, err
	}
	if app.ID <= 0 || app.Events == nil || app.Permissions == nil {
		return AuthenticatedApp{}, fmt.Errorf("%w: authenticated App response is incomplete", ErrInvalidAPIResponse)
	}
	return app, nil
}

// GetAppWebhookConfig returns the authenticated App's webhook delivery configuration.
func (client *APIClient) GetAppWebhookConfig(ctx context.Context, appJWT string) (AppWebhookConfig, error) {
	var configuration AppWebhookConfig
	if err := client.doJSON(ctx, http.MethodGet, "/app/hook/config", appJWT, nil, &configuration); err != nil {
		return AppWebhookConfig{}, err
	}
	if len(configuration.InsecureSSL) != 0 && !validWebhookTLSMode(configuration.InsecureSSL) {
		return AppWebhookConfig{}, fmt.Errorf("%w: App webhook configuration response is incomplete", ErrInvalidAPIResponse)
	}
	return configuration, nil
}

func validWebhookTLSMode(value json.RawMessage) bool {
	switch strings.TrimSpace(string(value)) {
	case "0", "1", `"0"`, `"1"`:
		return true
	default:
		return false
	}
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

func (client *APIClient) GetIssue(ctx context.Context, installationToken, owner, repository string, issueNumber int) (Issue, error) {
	if err := validateRepository(owner, repository); err != nil {
		return Issue{}, err
	}
	if issueNumber <= 0 {
		return Issue{}, &ConfigurationError{Cause: ErrInvalidIssueNumber}
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(repository), issueNumber)
	var issue Issue
	metadata, err := client.doJSONWithMetadata(ctx, http.MethodGet, path, installationToken, nil, &issue)
	if err != nil {
		return Issue{}, err
	}
	expectedHTMLPath := fmt.Sprintf("/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(repository), issueNumber)
	if err := validateIssueResponse(issue, issueNumber, expectedHTMLPath); err != nil {
		return Issue{}, responseFailure(metadata, "issue_validation", "", err)
	}
	return issue, nil
}

func (client *APIClient) ListIssueComments(ctx context.Context, installationToken, owner, repository string, issueNumber int) ([]IssueComment, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if issueNumber <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidIssueNumber}
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", url.PathEscape(owner), url.PathEscape(repository), issueNumber)
	comments := make([]IssueComment, 0)
	seenPages := make(map[string]struct{})
	seenComments := make(map[int64]struct{})
	for path != "" {
		if _, exists := seenPages[path]; exists {
			return nil, fmt.Errorf("%w: repeated pagination link", ErrInvalidAPIResponse)
		}
		seenPages[path] = struct{}{}
		var page []IssueComment
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, installationToken, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, comment := range page {
			if comment.ID <= 0 || comment.NodeID == "" || strings.TrimSpace(comment.Body) == "" || !validResponseURL(comment.HTMLURL) || comment.CreatedAt.IsZero() || comment.UpdatedAt.IsZero() || !resourcePathMatches(comment.IssueURL, fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(repository), issueNumber)) {
				return nil, fmt.Errorf("%w: Issue comment response is incomplete or does not match request", ErrInvalidAPIResponse)
			}
			if _, exists := seenComments[comment.ID]; exists {
				return nil, fmt.Errorf("%w: duplicate Issue comment id", ErrInvalidAPIResponse)
			}
			seenComments[comment.ID] = struct{}{}
			comments = append(comments, comment)
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		path, err = client.paginationPath(next)
		if err != nil {
			return nil, err
		}
	}
	return comments, nil
}

func (client *APIClient) GetPullRequest(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int) (PullRequest, error) {
	if err := validateRepository(owner, repository); err != nil {
		return PullRequest{}, err
	}
	if pullRequestNumber <= 0 {
		return PullRequest{}, &ConfigurationError{Cause: ErrInvalidPullRequestNumber}
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	var pullRequest PullRequest
	if err := client.doJSON(ctx, http.MethodGet, path, installationToken, nil, &pullRequest); err != nil {
		return PullRequest{}, err
	}
	expectedHTMLPath := fmt.Sprintf("/%s/%s/pull/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	if pullRequest.ID <= 0 || pullRequest.NodeID == "" || pullRequest.Number != pullRequestNumber || strings.TrimSpace(pullRequest.Title) == "" || !validIssueState(pullRequest.State) || !resourcePathMatches(pullRequest.HTMLURL, expectedHTMLPath) || !validGitRefName(pullRequest.Head.Ref) || !validCommitSHA(pullRequest.Head.SHA) || !validGitRefName(pullRequest.Base.Ref) || !validCommitSHA(pullRequest.Base.SHA) {
		return PullRequest{}, fmt.Errorf("%w: Pull Request response is incomplete or does not match request", ErrInvalidAPIResponse)
	}
	return pullRequest, nil
}

func (client *APIClient) ListPullRequestFiles(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int) ([]PullRequestFile, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if pullRequestNumber <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidPullRequestNumber}
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	seen := make(map[string]struct{})
	return listPaginated(client, ctx, installationToken, path, func(file PullRequestFile) error {
		if !validRepositoryPath(file.Filename) || !validPullRequestFileStatus(file.Status) || file.Patch != nil && !validPullRequestFilePatch(*file.Patch) {
			return fmt.Errorf("%w: Pull Request file response is incomplete or invalid", ErrInvalidAPIResponse)
		}
		if _, exists := seen[file.Filename]; exists {
			return fmt.Errorf("%w: duplicate Pull Request file path", ErrInvalidAPIResponse)
		}
		seen[file.Filename] = struct{}{}
		return nil
	})
}

// ListPullRequests returns every open or closed Pull Request for the requested head branch.
func (client *APIClient) ListPullRequests(ctx context.Context, installationToken, owner, repository string, input ListPullRequestsRequest) ([]PullRequest, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if !validGitRefName(input.Head) || input.Base != "" && (!validGitRefName(input.Base) || input.Base == input.Head) {
		return nil, &ConfigurationError{Cause: ErrInvalidPullRequest}
	}
	query := url.Values{
		"head":     {owner + ":" + input.Head},
		"per_page": {"100"},
		"state":    {"all"},
	}
	if input.Base != "" {
		query.Set("base", input.Base)
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls?%s", url.PathEscape(owner), url.PathEscape(repository), query.Encode())
	seenIDs := make(map[int64]struct{})
	seenNumbers := make(map[int]struct{})
	return listPaginated(client, ctx, installationToken, path, func(pullRequest PullRequest) error {
		expectedHTMLPath := fmt.Sprintf("/%s/%s/pull/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequest.Number)
		if pullRequest.ID <= 0 || pullRequest.NodeID == "" || pullRequest.Number <= 0 || strings.TrimSpace(pullRequest.Title) == "" || !validIssueState(pullRequest.State) ||
			!resourcePathMatches(pullRequest.HTMLURL, expectedHTMLPath) || pullRequest.Head.Label != owner+":"+input.Head || pullRequest.Head.Ref != input.Head || !validCommitSHA(pullRequest.Head.SHA) ||
			!validGitRefName(pullRequest.Base.Ref) || pullRequest.Base.Label != owner+":"+pullRequest.Base.Ref || input.Base != "" && pullRequest.Base.Ref != input.Base || !validCommitSHA(pullRequest.Base.SHA) {
			return fmt.Errorf("%w: listed Pull Request response is incomplete or does not match request", ErrInvalidAPIResponse)
		}
		if _, exists := seenIDs[pullRequest.ID]; exists {
			return fmt.Errorf("%w: duplicate Pull Request id", ErrInvalidAPIResponse)
		}
		if _, exists := seenNumbers[pullRequest.Number]; exists {
			return fmt.Errorf("%w: duplicate Pull Request number", ErrInvalidAPIResponse)
		}
		seenIDs[pullRequest.ID] = struct{}{}
		seenNumbers[pullRequest.Number] = struct{}{}
		return nil
	})
}

func (client *APIClient) ListPullRequestReviews(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int) ([]Review, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if pullRequestNumber <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidPullRequestNumber}
	}
	expectedPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	seen := make(map[int64]struct{})
	return listPaginated(client, ctx, installationToken, path, func(review Review) error {
		if review.ID <= 0 || review.NodeID == "" || !validReviewState(review.State) || review.State != "PENDING" && review.SubmittedAt.IsZero() || !validResponseURL(review.HTMLURL) || !resourcePathMatches(review.PullRequestURL, expectedPath) || review.CommitID != "" && !validCommitSHA(review.CommitID) {
			return fmt.Errorf("%w: Pull Request review response is incomplete or does not match request", ErrInvalidAPIResponse)
		}
		if _, exists := seen[review.ID]; exists {
			return fmt.Errorf("%w: duplicate Pull Request review id", ErrInvalidAPIResponse)
		}
		seen[review.ID] = struct{}{}
		return nil
	})
}

func (client *APIClient) ListReviewThreads(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int) ([]ReviewThread, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if pullRequestNumber <= 0 {
		return nil, &ConfigurationError{Cause: ErrInvalidPullRequestNumber}
	}

	threads := make([]ReviewThread, 0)
	seenNodeIDs := make(map[string]struct{})
	seenCommentDatabaseIDs := make(map[int64]struct{})
	commentDatabaseIDByNodeID := make(map[string]int64)
	seenThreadCursors := make(map[string]struct{})
	var threadCursor *string
	totalThreads := -1
	var repositoryID, pullRequestID string
	var lastMetadata responseMetadata
	for {
		var data reviewThreadsQueryData
		metadata, err := client.doGraphQL(ctx, installationToken, reviewThreadsQuery, map[string]any{
			"owner": owner, "name": repository, "number": pullRequestNumber, "threadsCursor": threadCursor,
		}, &data)
		if err != nil {
			return nil, reviewThreadFailure(ReviewThreadsQuery, metadata, err)
		}
		lastMetadata = metadata
		if err := validateReviewThreadsIdentity(data.Repository, owner, repository, pullRequestNumber, repositoryID, pullRequestID); err != nil {
			return nil, reviewThreadFailure(ReviewThreadsValidation, metadata, err)
		}
		if repositoryID == "" {
			repositoryID = data.Repository.ID
			pullRequestID = data.Repository.PullRequest.ID
		}

		connection := data.Repository.PullRequest.ReviewThreads
		if totalThreads < 0 {
			totalThreads = *connection.TotalCount
		} else if *connection.TotalCount != totalThreads {
			return nil, reviewThreadFailure(ReviewThreadsValidation, metadata, validationError("count_changed", "review_threads.totalCount"))
		}
		if err := validateGraphQLPageInfo(connection.PageInfo, len(connection.Nodes), threadCursor); err != nil {
			return nil, reviewThreadFailure(ReviewThreadsValidation, metadata, err)
		}
		for _, node := range connection.Nodes {
			thread, err := client.reviewThreadFromGraphQL(ctx, installationToken, owner, repository, pullRequestNumber, repositoryID, pullRequestID, metadata, node, seenNodeIDs, seenCommentDatabaseIDs, commentDatabaseIDByNodeID)
			if err != nil {
				var staged *ReviewThreadReadError
				if errors.As(err, &staged) {
					return nil, err
				}
				return nil, reviewThreadFailure(ReviewCommentsValidation, metadata, err)
			}
			threads = append(threads, thread)
		}
		if !*connection.PageInfo.HasNextPage {
			break
		}
		next := *connection.PageInfo.EndCursor
		if _, exists := seenThreadCursors[next]; exists {
			return nil, reviewThreadFailure(ReviewThreadsValidation, metadata, validationError("repeated_cursor", "page_info.endCursor"))
		}
		seenThreadCursors[next] = struct{}{}
		threadCursor = &next
	}
	if totalThreads != len(threads) {
		return nil, reviewThreadFailure(ReviewThreadsValidation, lastMetadata, validationError("count_mismatch", "review_threads.totalCount"))
	}
	return threads, nil
}

const reviewCommentFields = `
  id
  fullDatabaseId
  body
  path
  commit { oid }
  originalCommit { oid }
  url
  author { login }
  createdAt
  updatedAt
  line
  startLine
  originalLine
  originalStartLine
  position
  subjectType
  pullRequestReview { fullDatabaseId }
  replyTo { id fullDatabaseId }
`

const reviewThreadsQuery = `query ReviewThreads($owner: String!, $name: String!, $number: Int!, $threadsCursor: String) {
  repository(owner: $owner, name: $name) {
    id
    name
    owner { login }
    pullRequest(number: $number) {
      id
      number
      repository { id name owner { login } }
      reviewThreads(first: 100, after: $threadsCursor) {
        totalCount
        pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
        nodes {
          id
          isResolved
          isOutdated
          path
          diffSide
          startDiffSide
          line
          startLine
          originalLine
          originalStartLine
          subjectType
          comments(first: 100) {
            totalCount
            pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
            nodes {` + reviewCommentFields + `}
          }
        }
      }
    }
  }
}`

const reviewThreadCommentsQuery = `query ReviewThreadComments($threadID: ID!, $commentsCursor: String!) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      id
      isResolved
      isOutdated
      path
      diffSide
      startDiffSide
      line
      startLine
      originalLine
      originalStartLine
      subjectType
      pullRequest { id number repository { id name owner { login } } }
      comments(first: 100, after: $commentsCursor) {
        totalCount
        pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
        nodes {` + reviewCommentFields + `}
      }
    }
  }
}`

type graphQLPageInfo struct {
	HasNextPage     *bool   `json:"hasNextPage"`
	HasPreviousPage *bool   `json:"hasPreviousPage"`
	StartCursor     *string `json:"startCursor"`
	EndCursor       *string `json:"endCursor"`
}

type graphQLRepositoryIdentity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner *struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type graphQLPullRequestIdentity struct {
	ID         string                     `json:"id"`
	Number     *int                       `json:"number"`
	Repository *graphQLRepositoryIdentity `json:"repository"`
}

type graphQLReviewComment struct {
	ID             string             `json:"id"`
	FullDatabaseID *graphQLDatabaseID `json:"fullDatabaseId"`
	Body           string             `json:"body"`
	Path           string             `json:"path"`
	Commit         *struct {
		OID string `json:"oid"`
	} `json:"commit"`
	OriginalCommit *struct {
		OID string `json:"oid"`
	} `json:"originalCommit"`
	URL    string `json:"url"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	CreatedAt         *time.Time `json:"createdAt"`
	UpdatedAt         *time.Time `json:"updatedAt"`
	Line              *int       `json:"line"`
	StartLine         *int       `json:"startLine"`
	OriginalLine      *int       `json:"originalLine"`
	OriginalStartLine *int       `json:"originalStartLine"`
	Position          *int       `json:"position"`
	SubjectType       string     `json:"subjectType"`
	PullRequestReview *struct {
		FullDatabaseID *graphQLDatabaseID `json:"fullDatabaseId"`
	} `json:"pullRequestReview"`
	ReplyTo *struct {
		ID             string             `json:"id"`
		FullDatabaseID *graphQLDatabaseID `json:"fullDatabaseId"`
	} `json:"replyTo"`
}

type graphQLDatabaseID int64

func (id *graphQLDatabaseID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil || !validDecimal(value) {
		return errors.New("invalid GraphQL BigInt")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return errors.New("invalid GraphQL BigInt")
	}
	*id = graphQLDatabaseID(parsed)
	return nil
}

type graphQLReviewCommentConnection struct {
	TotalCount *int                    `json:"totalCount"`
	PageInfo   *graphQLPageInfo        `json:"pageInfo"`
	Nodes      []*graphQLReviewComment `json:"nodes"`
}

type graphQLReviewThread struct {
	ID                string                          `json:"id"`
	IsResolved        *bool                           `json:"isResolved"`
	IsOutdated        *bool                           `json:"isOutdated"`
	Path              string                          `json:"path"`
	DiffSide          string                          `json:"diffSide"`
	StartDiffSide     *string                         `json:"startDiffSide"`
	Line              *int                            `json:"line"`
	StartLine         *int                            `json:"startLine"`
	OriginalLine      *int                            `json:"originalLine"`
	OriginalStartLine *int                            `json:"originalStartLine"`
	SubjectType       string                          `json:"subjectType"`
	PullRequest       *graphQLPullRequestIdentity     `json:"pullRequest"`
	Comments          *graphQLReviewCommentConnection `json:"comments"`
}

type graphQLReviewThreadConnection struct {
	TotalCount *int                   `json:"totalCount"`
	PageInfo   *graphQLPageInfo       `json:"pageInfo"`
	Nodes      []*graphQLReviewThread `json:"nodes"`
}

type reviewThreadsQueryData struct {
	Repository *struct {
		graphQLRepositoryIdentity
		PullRequest *struct {
			ID            string                         `json:"id"`
			Number        *int                           `json:"number"`
			Repository    *graphQLRepositoryIdentity     `json:"repository"`
			ReviewThreads *graphQLReviewThreadConnection `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type reviewThreadCommentsQueryData struct {
	Node *graphQLReviewThread `json:"node"`
}

func validateReviewThreadsIdentity(actual *struct {
	graphQLRepositoryIdentity
	PullRequest *struct {
		ID            string                         `json:"id"`
		Number        *int                           `json:"number"`
		Repository    *graphQLRepositoryIdentity     `json:"repository"`
		ReviewThreads *graphQLReviewThreadConnection `json:"reviewThreads"`
	} `json:"pullRequest"`
}, owner, repository string, pullRequestNumber int, expectedRepositoryID, expectedPullRequestID string) error {
	switch {
	case actual == nil:
		return validationError("missing_field", "repository")
	case !validGraphQLNodeID(actual.ID):
		return validationError("invalid_node_id", "repository.id")
	case actual.Name != repository:
		return validationError("identity_mismatch", "repository.name")
	case actual.Owner == nil:
		return validationError("missing_field", "repository.owner")
	case actual.Owner.Login != owner:
		return validationError("identity_mismatch", "repository.owner.login")
	case actual.PullRequest == nil:
		return validationError("missing_field", "pull_request")
	case !validGraphQLNodeID(actual.PullRequest.ID):
		return validationError("invalid_node_id", "pull_request.id")
	case actual.PullRequest.Number == nil:
		return validationError("missing_field", "pull_request.number")
	case *actual.PullRequest.Number != pullRequestNumber:
		return validationError("identity_mismatch", "pull_request.number")
	}
	if err := validateGraphQLRepositoryIdentity(actual.PullRequest.Repository, owner, repository, actual.ID); err != nil {
		return err
	}
	connection := actual.PullRequest.ReviewThreads
	switch {
	case connection == nil:
		return validationError("missing_field", "review_threads")
	case connection.TotalCount == nil:
		return validationError("missing_field", "review_threads.totalCount")
	case *connection.TotalCount < 0:
		return validationError("invalid_count", "review_threads.totalCount")
	case connection.Nodes == nil:
		return validationError("missing_field", "review_threads.nodes")
	case len(connection.Nodes) > 100:
		return validationError("oversized_page", "review_threads.nodes")
	case *connection.TotalCount < len(connection.Nodes):
		return validationError("count_mismatch", "review_threads.totalCount")
	case expectedRepositoryID != "" && actual.ID != expectedRepositoryID:
		return validationError("identity_changed", "repository.id")
	case expectedRepositoryID != "" && actual.PullRequest.ID != expectedPullRequestID:
		return validationError("identity_changed", "pull_request.id")
	}
	return nil
}

func (client *APIClient) reviewThreadFromGraphQL(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int, repositoryID, pullRequestID string, metadata responseMetadata, node *graphQLReviewThread, seenNodeIDs map[string]struct{}, seenCommentDatabaseIDs map[int64]struct{}, commentDatabaseIDByNodeID map[string]int64) (ReviewThread, error) {
	if err := validateGraphQLReviewThread(node); err != nil {
		return ReviewThread{}, reviewThreadFailure(ReviewThreadsValidation, metadata, err)
	}
	if _, exists := seenNodeIDs[node.ID]; exists {
		return ReviewThread{}, reviewThreadFailure(ReviewThreadsValidation, metadata, validationError("duplicate_id", "review_thread.id"))
	}
	seenNodeIDs[node.ID] = struct{}{}

	thread := ReviewThread{ID: node.ID, Path: node.Path, Resolved: *node.IsResolved, Outdated: *node.IsOutdated, Comments: make([]ReviewComment, 0)}
	connection := node.Comments
	pullRequestURL := strings.TrimRight(client.baseURL, "/") + fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	pullRequestHTMLPath := fmt.Sprintf("/%s/%s/pull/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	var cursor *string
	seenCursors := make(map[string]struct{})
	for {
		if err := validateGraphQLPageInfo(connection.PageInfo, len(connection.Nodes), cursor); err != nil {
			return ReviewThread{}, reviewThreadFailure(ReviewCommentsValidation, metadata, err)
		}
		for _, graphQLComment := range connection.Nodes {
			comment, err := reviewCommentFromGraphQL(graphQLComment, node, pullRequestURL, pullRequestHTMLPath, seenNodeIDs, seenCommentDatabaseIDs, commentDatabaseIDByNodeID)
			if err != nil {
				return ReviewThread{}, reviewThreadFailure(ReviewCommentsValidation, metadata, err)
			}
			thread.Comments = append(thread.Comments, comment)
		}
		if !*connection.PageInfo.HasNextPage {
			break
		}
		next := *connection.PageInfo.EndCursor
		if _, exists := seenCursors[next]; exists {
			return ReviewThread{}, reviewThreadFailure(ReviewCommentsValidation, metadata, validationError("repeated_cursor", "page_info.endCursor"))
		}
		seenCursors[next] = struct{}{}
		cursor = &next

		var data reviewThreadCommentsQueryData
		continuation, err := client.doGraphQL(ctx, installationToken, reviewThreadCommentsQuery, map[string]any{"threadID": node.ID, "commentsCursor": next}, &data)
		if err != nil {
			return ReviewThread{}, reviewThreadFailure(ReviewCommentsQuery, continuation, err)
		}
		metadata = continuation
		if err := validateReviewThreadContinuation(node, data.Node, owner, repository, pullRequestNumber, repositoryID, pullRequestID); err != nil {
			return ReviewThread{}, reviewThreadFailure(ReviewThreadsValidation, metadata, err)
		}
		connection = data.Node.Comments
	}
	if len(thread.Comments) != *node.Comments.TotalCount || len(thread.Comments) == 0 {
		return ReviewThread{}, reviewThreadFailure(ReviewCommentsValidation, metadata, validationError("count_mismatch", "review_comments.totalCount"))
	}
	if err := validateReviewThreadReplies(thread.Comments); err != nil {
		return ReviewThread{}, reviewThreadFailure(ReviewCommentsValidation, metadata, err)
	}
	return thread, nil
}

func validateGraphQLReviewThread(node *graphQLReviewThread) error {
	switch {
	case node == nil:
		return validationError("missing_field", "review_thread")
	case !validGraphQLNodeID(node.ID):
		return validationError("invalid_node_id", "review_thread.id")
	case node.IsResolved == nil:
		return validationError("missing_field", "review_thread.isResolved")
	case node.IsOutdated == nil:
		return validationError("missing_field", "review_thread.isOutdated")
	case !validRepositoryPath(node.Path):
		return validationError("invalid_path", "review_thread.path")
	case node.Comments == nil:
		return validationError("missing_field", "review_comments")
	case node.Comments.TotalCount == nil:
		return validationError("missing_field", "review_comments.totalCount")
	case *node.Comments.TotalCount < 1:
		return validationError("invalid_count", "review_comments.totalCount")
	case node.Comments.Nodes == nil:
		return validationError("missing_field", "review_comments.nodes")
	case len(node.Comments.Nodes) > 100:
		return validationError("oversized_page", "review_comments.nodes")
	case *node.Comments.TotalCount < len(node.Comments.Nodes):
		return validationError("count_mismatch", "review_comments.totalCount")
	}
	return validateGraphQLReviewThreadLocation(node)
}

func reviewCommentFromGraphQL(source *graphQLReviewComment, thread *graphQLReviewThread, pullRequestURL, pullRequestHTMLPath string, seenNodeIDs map[string]struct{}, seenDatabaseIDs map[int64]struct{}, databaseIDByNodeID map[string]int64) (ReviewComment, error) {
	if err := validateGraphQLReviewComment(source, thread, pullRequestHTMLPath); err != nil {
		return ReviewComment{}, err
	}
	if _, exists := seenNodeIDs[source.ID]; exists {
		return ReviewComment{}, validationError("duplicate_id", "review_comment.id")
	}
	databaseID := int64(*source.FullDatabaseID)
	if _, exists := seenDatabaseIDs[databaseID]; exists {
		return ReviewComment{}, validationError("duplicate_id", "review_comment.fullDatabaseId")
	}
	if err := validateGraphQLReviewCommentLocation(source, thread); err != nil {
		return ReviewComment{}, err
	}
	seenNodeIDs[source.ID] = struct{}{}
	seenDatabaseIDs[databaseID] = struct{}{}

	inReplyToID := int64(0)
	if source.ReplyTo != nil {
		if !validGraphQLNodeID(source.ReplyTo.ID) {
			return ReviewComment{}, validationError("invalid_node_id", "review_comment.replyTo.id")
		}
		if source.ReplyTo.FullDatabaseID == nil {
			return ReviewComment{}, validationError("missing_field", "review_comment.replyTo.fullDatabaseId")
		}
		if *source.ReplyTo.FullDatabaseID <= 0 {
			return ReviewComment{}, validationError("invalid_id", "review_comment.replyTo.fullDatabaseId")
		}
		replyDatabaseID := int64(*source.ReplyTo.FullDatabaseID)
		if knownDatabaseID, exists := databaseIDByNodeID[source.ReplyTo.ID]; !exists {
			return ReviewComment{}, validationError("missing_target", "review_comment.replyTo")
		} else if knownDatabaseID != replyDatabaseID {
			return ReviewComment{}, validationError("identity_mismatch", "review_comment.replyTo.fullDatabaseId")
		}
		inReplyToID = replyDatabaseID
	}
	databaseIDByNodeID[source.ID] = databaseID
	var pullRequestReviewID *int64
	if source.PullRequestReview != nil {
		reviewID := int64(*source.PullRequestReview.FullDatabaseID)
		pullRequestReviewID = &reviewID
	}
	startSide := ""
	if thread.StartDiffSide != nil {
		startSide = *thread.StartDiffSide
	}
	return ReviewComment{
		ID: databaseID, NodeID: source.ID, PullRequestReviewID: pullRequestReviewID, InReplyToID: inReplyToID,
		Body: source.Body, Path: source.Path, CommitID: source.Commit.OID, OriginalCommitID: source.OriginalCommit.OID, HTMLURL: source.URL,
		PullRequestURL: pullRequestURL,
		User:           User{Login: source.Author.Login}, CreatedAt: *source.CreatedAt, UpdatedAt: *source.UpdatedAt, Line: source.Line, StartLine: normalizedGraphQLReviewThreadStartLine(thread),
		OriginalLine: source.OriginalLine, OriginalStartLine: source.OriginalStartLine, Side: thread.DiffSide, StartSide: startSide, SubjectType: strings.ToLower(source.SubjectType),
		Position: source.Position,
	}, nil
}

func validateGraphQLReviewCommentLocation(comment *graphQLReviewComment, thread *graphQLReviewThread) error {
	switch {
	case comment.Position != nil && *comment.Position <= 0:
		return validationError("invalid_position", "review_comment.position")
	case comment.Position != nil && thread.SubjectType != "LINE":
		return validationError("position_on_file", "review_comment.position")
	case comment.SubjectType != thread.SubjectType:
		return validationError("location_mismatch", "review_comment.subjectType")
	case !equalOptionalInt(comment.Line, thread.Line):
		return validationError("location_mismatch", "review_comment.line")
	case !equalOptionalInt(comment.StartLine, normalizedGraphQLReviewThreadStartLine(thread)):
		return validationError("location_mismatch", "review_comment.startLine")
	case !equalOptionalInt(comment.OriginalLine, thread.OriginalLine):
		return validationError("location_mismatch", "review_comment.originalLine")
	case !equalOptionalInt(comment.OriginalStartLine, thread.OriginalStartLine):
		return validationError("location_mismatch", "review_comment.originalStartLine")
	default:
		return nil
	}
}

func validateGraphQLReviewThreadLocation(thread *graphQLReviewThread) error {
	if thread.DiffSide != "LEFT" && thread.DiffSide != "RIGHT" {
		return validationError("invalid_diff_side", "review_thread.diffSide")
	}
	switch thread.SubjectType {
	case "FILE":
		switch {
		case thread.Line != nil:
			return validationError("file_has_line", "review_thread.line")
		case thread.StartLine != nil:
			return validationError("file_has_line", "review_thread.startLine")
		case thread.OriginalLine != nil:
			return validationError("file_has_line", "review_thread.originalLine")
		case thread.OriginalStartLine != nil:
			return validationError("file_has_line", "review_thread.originalStartLine")
		case thread.StartDiffSide != nil:
			return validationError("file_has_diff_side", "review_thread.startDiffSide")
		}
	case "LINE":
		switch {
		case thread.OriginalLine == nil:
			return validationError("missing_field", "review_thread.originalLine")
		case *thread.OriginalLine <= 0:
			return validationError("invalid_line", "review_thread.originalLine")
		case thread.OriginalStartLine != nil && *thread.OriginalStartLine <= 0:
			return validationError("invalid_line", "review_thread.originalStartLine")
		case thread.OriginalStartLine != nil && *thread.OriginalStartLine >= *thread.OriginalLine:
			return validationError("invalid_range", "review_thread.originalStartLine")
		}
		if *thread.IsOutdated {
			if thread.Line != nil {
				return validationError("outdated_has_line", "review_thread.line")
			}
			if thread.StartLine != nil {
				return validationError("outdated_has_line", "review_thread.startLine")
			}
		} else if thread.Line == nil {
			return validationError("missing_field", "review_thread.line")
		} else if *thread.Line <= 0 {
			return validationError("invalid_line", "review_thread.line")
		}
		if thread.OriginalStartLine == nil {
			if thread.StartDiffSide != nil {
				return validationError("single_line_has_diff_side", "review_thread.startDiffSide")
			}
			if normalizedGraphQLReviewThreadStartLine(thread) != nil {
				return validationError("single_line_has_start_line", "review_thread.startLine")
			}
			return nil
		}
		if thread.StartDiffSide == nil {
			return validationError("missing_field", "review_thread.startDiffSide")
		}
		if *thread.StartDiffSide != "LEFT" && *thread.StartDiffSide != "RIGHT" {
			return validationError("invalid_diff_side", "review_thread.startDiffSide")
		}
		if !*thread.IsOutdated {
			switch {
			case thread.StartLine == nil:
				return validationError("missing_field", "review_thread.startLine")
			case *thread.StartLine <= 0:
				return validationError("invalid_line", "review_thread.startLine")
			case *thread.StartLine >= *thread.Line:
				return validationError("invalid_range", "review_thread.startLine")
			}
		}
	default:
		return validationError("invalid_subject_type", "review_thread.subjectType")
	}
	return nil
}

func normalizedGraphQLReviewThreadStartLine(thread *graphQLReviewThread) *int {
	if !*thread.IsOutdated && thread.SubjectType == "LINE" && thread.StartDiffSide == nil && thread.OriginalStartLine == nil &&
		thread.Line != nil && thread.StartLine != nil && *thread.StartLine == *thread.Line {
		return nil
	}
	return thread.StartLine
}

func equalOptionalInt(first, second *int) bool {
	return first == nil && second == nil || first != nil && second != nil && *first == *second
}

func equalOptionalString(first, second *string) bool {
	return first == nil && second == nil || first != nil && second != nil && *first == *second
}

func validateReviewThreadReplies(comments []ReviewComment) error {
	root := comments[0]
	if root.InReplyToID != 0 {
		return validationError("root_is_reply", "review_comment.replyTo")
	}
	for _, comment := range comments[1:] {
		if comment.InReplyToID != root.ID {
			return validationError("reply_not_to_root", "review_comment.replyTo")
		}
	}
	return nil
}

func validateGraphQLPageInfo(pageInfo *graphQLPageInfo, nodeCount int, after *string) error {
	switch {
	case pageInfo == nil:
		return validationError("missing_field", "page_info")
	case pageInfo.HasNextPage == nil:
		return validationError("missing_field", "page_info.hasNextPage")
	case pageInfo.HasPreviousPage == nil:
		return validationError("missing_field", "page_info.hasPreviousPage")
	case nodeCount < 0 || nodeCount > 100:
		return validationError("invalid_count", "page_info")
	}
	if nodeCount == 0 {
		switch {
		case after != nil:
			return validationError("empty_continuation", "page_info")
		case pageInfo.StartCursor != nil:
			return validationError("empty_page_has_cursor", "page_info.startCursor")
		case pageInfo.EndCursor != nil:
			return validationError("empty_page_has_cursor", "page_info.endCursor")
		case *pageInfo.HasNextPage:
			return validationError("empty_page_has_next", "page_info.hasNextPage")
		}
		return nil
	}
	switch {
	case pageInfo.StartCursor == nil:
		return validationError("missing_field", "page_info.startCursor")
	case pageInfo.EndCursor == nil:
		return validationError("missing_field", "page_info.endCursor")
	case !validGraphQLCursor(*pageInfo.StartCursor):
		return validationError("invalid_cursor", "page_info.startCursor")
	case !validGraphQLCursor(*pageInfo.EndCursor):
		return validationError("invalid_cursor", "page_info.endCursor")
	case after != nil && *pageInfo.StartCursor == *after:
		return validationError("repeated_cursor", "page_info.startCursor")
	case after != nil && *pageInfo.EndCursor == *after:
		return validationError("repeated_cursor", "page_info.endCursor")
	}
	return nil
}

func graphQLResourcePathMatches(rawURL, expectedPath string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && validParsedResponseURL(parsed) && parsed.EscapedPath() == expectedPath
}

func validGraphQLNodeID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func validGraphQLCursor(value string) bool {
	return validGraphQLNodeID(value)
}

func (client *APIClient) doGraphQL(ctx context.Context, installationToken, query string, variables map[string]any, destination any) (responseMetadata, error) {
	var response struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	metadata, err := client.doJSONWithMetadata(ctx, http.MethodPost, "/graphql", installationToken, struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{Query: query, Variables: variables}, &response)
	if err != nil {
		return metadata, err
	}
	if len(response.Errors) != 0 {
		return metadata, responseFailure(metadata, "graphql_envelope", "graphql_errors", &GraphQLQueryError{RequestID: metadata.requestID()})
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return metadata, responseFailure(metadata, "graphql_envelope", "missing_data", &GraphQLQueryError{RequestID: metadata.requestID()})
	}
	if err := json.Unmarshal(response.Data, destination); err != nil {
		return metadata, responseFailure(metadata, "graphql_data_decoding", "invalid_json", ErrInvalidAPIResponse)
	}
	return metadata, nil
}

func (client *APIClient) GetCheckRuns(ctx context.Context, installationToken, owner, repository, commitSHA string) ([]CheckRun, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if !validCommitSHA(commitSHA) {
		return nil, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", url.PathEscape(owner), url.PathEscape(repository), commitSHA)
	checks := make([]CheckRun, 0)
	seenPages := make(map[string]struct{})
	seenChecks := make(map[int64]struct{})
	totalCount := -1
	for path != "" {
		if _, exists := seenPages[path]; exists {
			return nil, fmt.Errorf("%w: repeated pagination link", ErrInvalidAPIResponse)
		}
		seenPages[path] = struct{}{}
		var page struct {
			TotalCount *int       `json:"total_count"`
			CheckRuns  []CheckRun `json:"check_runs"`
		}
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, installationToken, nil, &page)
		if err != nil {
			return nil, err
		}
		if page.TotalCount == nil || *page.TotalCount < len(page.CheckRuns) || totalCount > *page.TotalCount {
			return nil, fmt.Errorf("%w: check-run count is invalid", ErrInvalidAPIResponse)
		}
		totalCount = *page.TotalCount
		for _, check := range page.CheckRuns {
			if check.ID <= 0 || check.NodeID == "" || strings.TrimSpace(check.Name) == "" || check.HeadSHA != commitSHA || !validCheckRunStatus(check.Status) || !validCheckRunResult(check) || !validResponseURL(check.HTMLURL) || check.DetailsURL != "" && !validResponseURL(check.DetailsURL) {
				return nil, fmt.Errorf("%w: check-run response is incomplete or does not match requested commit", ErrInvalidAPIResponse)
			}
			if _, exists := seenChecks[check.ID]; exists {
				return nil, fmt.Errorf("%w: duplicate check-run id", ErrInvalidAPIResponse)
			}
			seenChecks[check.ID] = struct{}{}
			checks = append(checks, check)
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		path, err = client.paginationPath(next)
		if err != nil {
			return nil, err
		}
	}
	if totalCount != len(checks) {
		return nil, fmt.Errorf("%w: check-run count does not match paginated results", ErrInvalidAPIResponse)
	}
	return checks, nil
}

func (client *APIClient) OpenPullRequest(ctx context.Context, installationToken, owner, repository string, input OpenPullRequestRequest) (PullRequest, error) {
	if err := validateRepository(owner, repository); err != nil {
		return PullRequest{}, err
	}
	if strings.TrimSpace(input.Title) == "" || !validGitRefName(input.Head) || !validGitRefName(input.Base) || input.Head == input.Base {
		return PullRequest{}, &ConfigurationError{Cause: ErrInvalidPullRequest}
	}
	if input.IssueNumber <= 0 {
		return PullRequest{}, &ConfigurationError{Cause: ErrInvalidIssueNumber}
	}
	if !validMarker(input.Marker) {
		return PullRequest{}, &ConfigurationError{Cause: ErrInvalidMarker}
	}
	body := JoinBodyParts(input.Body, fmt.Sprintf("Closes #%d", input.IssueNumber), input.Marker)
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}{Title: input.Title, Body: body, Head: input.Head, Base: input.Base}
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repository))
	var pullRequest PullRequest
	if err := client.doJSON(ctx, http.MethodPost, path, installationToken, payload, &pullRequest); err != nil {
		return PullRequest{}, err
	}
	if pullRequest.ID <= 0 || pullRequest.NodeID == "" || pullRequest.Number <= 0 || pullRequest.Title != input.Title || pullRequest.Body != body || pullRequest.State != "open" || !resourcePathMatches(pullRequest.HTMLURL, fmt.Sprintf("/%s/%s/pull/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequest.Number)) || pullRequest.Head.Ref != input.Head || !validCommitSHA(pullRequest.Head.SHA) || pullRequest.Base.Ref != input.Base || !validCommitSHA(pullRequest.Base.SHA) {
		return PullRequest{}, fmt.Errorf("%w: opened Pull Request response is incomplete or does not match request", ErrInvalidAPIResponse)
	}
	return pullRequest, nil
}

func (client *APIClient) CreateIssueComment(ctx context.Context, installationToken, owner, repository string, issueNumber int, input CommentRequest) (IssueComment, error) {
	if issueNumber <= 0 {
		return IssueComment{}, &ConfigurationError{Cause: ErrInvalidIssueNumber}
	}
	return client.createComment(ctx, installationToken, owner, repository, issueNumber, input)
}

func (client *APIClient) CreatePullRequestComment(ctx context.Context, installationToken, owner, repository string, pullRequestNumber int, input CommentRequest) (IssueComment, error) {
	if pullRequestNumber <= 0 {
		return IssueComment{}, &ConfigurationError{Cause: ErrInvalidPullRequestNumber}
	}
	return client.createComment(ctx, installationToken, owner, repository, pullRequestNumber, input)
}

// DeleteIssueComment deletes an Issue or Pull Request comment by its shared issue-comment ID.
func (client *APIClient) DeleteIssueComment(ctx context.Context, installationToken, owner, repository string, commentID int64) error {
	if err := validateRepository(owner, repository); err != nil {
		return err
	}
	if commentID <= 0 {
		return &ConfigurationError{Cause: ErrInvalidComment}
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", url.PathEscape(owner), url.PathEscape(repository), commentID)
	return client.doJSON(ctx, http.MethodDelete, path, installationToken, nil, nil)
}

func (client *APIClient) createComment(ctx context.Context, installationToken, owner, repository string, number int, input CommentRequest) (IssueComment, error) {
	if err := validateRepository(owner, repository); err != nil {
		return IssueComment{}, err
	}
	if strings.TrimSpace(input.Body) == "" && strings.TrimSpace(input.Marker) == "" {
		return IssueComment{}, &ConfigurationError{Cause: ErrInvalidComment}
	}
	if !validMarker(input.Marker) {
		return IssueComment{}, &ConfigurationError{Cause: ErrInvalidMarker}
	}
	body := JoinBodyParts(input.Body, input.Marker)
	payload := struct {
		Body string `json:"body"`
	}{Body: body}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(repository), number)
	var comment IssueComment
	if err := client.doJSON(ctx, http.MethodPost, path, installationToken, payload, &comment); err != nil {
		return IssueComment{}, err
	}
	expectedPath := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(repository), number)
	if comment.ID <= 0 || comment.NodeID == "" || comment.Body != body || !validResponseURL(comment.HTMLURL) || comment.CreatedAt.IsZero() || comment.UpdatedAt.IsZero() || !resourcePathMatches(comment.IssueURL, expectedPath) {
		return IssueComment{}, fmt.Errorf("%w: created comment response is incomplete or does not match request", ErrInvalidAPIResponse)
	}
	return comment, nil
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
	if !validCommitSHA(review.CommitID) {
		return Review{}, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}
	for _, comment := range review.Comments {
		if !validReviewCommentRequest(comment) {
			return Review{}, &ConfigurationError{Cause: ErrInvalidReview}
		}
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	var submitted Review
	if err := client.doJSON(ctx, http.MethodPost, path, installationToken, review, &submitted); err != nil {
		return Review{}, err
	}
	expectedState := "APPROVED"
	if review.Event == ReviewRequestChanges {
		expectedState = "CHANGES_REQUESTED"
	}
	expectedPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repository), pullRequestNumber)
	if submitted.ID <= 0 || submitted.NodeID == "" || submitted.State != expectedState || submitted.CommitID != review.CommitID || !validResponseURL(submitted.HTMLURL) || !resourcePathMatches(submitted.PullRequestURL, expectedPath) || submitted.SubmittedAt.IsZero() {
		return Review{}, fmt.Errorf("%w: submitted review response is incomplete or does not match request", ErrInvalidAPIResponse)
	}
	return submitted, nil
}

func listPaginated[T any](client *APIClient, ctx context.Context, credential, firstPath string, validate func(T) error) ([]T, error) {
	items := make([]T, 0)
	path := firstPath
	seen := make(map[string]struct{})
	for path != "" {
		if _, exists := seen[path]; exists {
			return nil, fmt.Errorf("%w: repeated pagination link", ErrInvalidAPIResponse)
		}
		seen[path] = struct{}{}
		var page []T
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, credential, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, item := range page {
			if err := validate(item); err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		path, err = client.paginationPath(next)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
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
	metadata, err := client.doJSONWithMetadata(ctx, method, path, credential, body, destination)
	return metadata.header, err
}

func (client *APIClient) doJSONWithMetadata(ctx context.Context, method, path, credential string, body, destination any) (metadata responseMetadata, err error) {
	if strings.TrimSpace(credential) == "" {
		return metadata, &ConfigurationError{Cause: ErrMissingCredential}
	}
	defer func() {
		if err != nil {
			err = redactAPIClientError(err, credential)
		}
	}()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return metadata, fmt.Errorf("encode GitHub API request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, requestBody)
	if err != nil {
		return metadata, fmt.Errorf("create GitHub API request: %w", err)
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
			return metadata, responseFailure(metadata, "github_http", CancellationReason(ctx.Err()), ctx.Err())
		}
		reason := CancellationReason(err)
		if reason == "" {
			reason = "transport_failed"
		}
		return metadata, responseFailure(metadata, "github_http", reason, &TransientError{Cause: err})
	}
	defer response.Body.Close()
	metadata = responseMetadata{header: response.Header.Clone(), status: response.StatusCode}
	if credential != "" && strings.Contains(metadata.requestID(), credential) {
		metadata.header.Del("X-GitHub-Request-Id")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return metadata, responseFailure(metadata, "github_http", "http_rejected", classifyAPIError(response, method, path))
	}
	if destination == nil || response.StatusCode == http.StatusNoContent {
		return metadata, nil
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		reason := CancellationReason(err)
		if reason == "" {
			reason = "invalid_json"
		}
		return metadata, responseFailure(metadata, "response_decoding", reason, ErrInvalidAPIResponse)
	}
	return metadata, nil
}

func redactAPIClientError(err error, credential string) error {
	if diagnostic, ok := err.(*diagnosticError); ok {
		copy := *diagnostic
		copy.cause = redactAPIClientError(diagnostic.cause, credential)
		if credential != "" && strings.Contains(copy.detail.RequestID, credential) {
			copy.detail.RequestID = ""
		}
		return &copy
	}
	if credential == "" || !strings.Contains(err.Error(), credential) {
		return err
	}
	redact := func(value string) string { return strings.ReplaceAll(value, credential, "[REDACTED]") }
	copyAPIError := func(source *APIError) *APIError {
		if source == nil {
			return nil
		}
		return &APIError{StatusCode: source.StatusCode, Method: source.Method, Path: source.Path, Message: redact(source.Message), RequestID: redact(source.RequestID)}
	}
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		return &RateLimitError{APIError: copyAPIError(rateLimit.APIError), RetryAfter: rateLimit.RetryAfter, ResetAt: rateLimit.ResetAt}
	}
	var permission *PermissionError
	if errors.As(err, &permission) {
		return &PermissionError{APIError: copyAPIError(permission.APIError)}
	}
	var transient *TransientError
	if errors.As(err, &transient) {
		var api *APIError
		if errors.As(transient.Cause, &api) {
			return &TransientError{Cause: copyAPIError(api)}
		}
		return &TransientError{Cause: errors.New(redact(transient.Cause.Error()))}
	}
	var apiError *APIError
	if errors.As(err, &apiError) {
		return copyAPIError(apiError)
	}
	return errors.New(redact(err.Error()))
}

func validateRepository(owner, repository string) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repository) == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") {
		return &ConfigurationError{Cause: ErrInvalidRepository}
	}
	return nil
}

func resourcePathMatches(rawURL, expectedSuffix string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && validParsedResponseURL(parsed) && (parsed.EscapedPath() == expectedSuffix || strings.HasSuffix(parsed.EscapedPath(), expectedSuffix))
}

func validResponseURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && validParsedResponseURL(parsed)
}

func validParsedResponseURL(parsed *url.URL) bool {
	return parsed != nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.User == nil
}

func validLabelName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 50
}

func validCheckRunStatus(status string) bool {
	switch status {
	case "queued", "in_progress", "completed", "waiting", "requested", "pending":
		return true
	default:
		return false
	}
}

func validIssueState(state string) bool {
	return state == "open" || state == "closed"
}

func validReviewState(state string) bool {
	switch state {
	case "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED", "PENDING":
		return true
	default:
		return false
	}
}

func validPullRequestFileStatus(status PullRequestFileStatus) bool {
	switch status {
	case PullRequestFileAdded, PullRequestFileRemoved, PullRequestFileModified, PullRequestFileRenamed, PullRequestFileCopied, PullRequestFileChanged, PullRequestFileUnchanged:
		return true
	default:
		return false
	}
}

func validPullRequestFilePatch(patch string) bool {
	lines := strings.Split(patch, "\n")
	if len(lines) < 2 {
		return false
	}
	oldLines, newLines := 0, 0
	oldCount, newCount, inHunk := 0, 0, false
	changed, previousWasBody := false, false
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if inHunk && (oldLines != oldCount || newLines != newCount || !changed) {
				return false
			}
			var valid bool
			oldCount, newCount, valid = parsePullRequestFileHunkHeader(line)
			if !valid {
				return false
			}
			oldLines, newLines, inHunk, changed, previousWasBody = 0, 0, true, false, false
			continue
		}
		if !inHunk {
			return false
		}
		if line == `\ No newline at end of file` {
			if !previousWasBody {
				return false
			}
			previousWasBody = false
			continue
		}
		if line == "" {
			return false
		}
		switch line[0] {
		case ' ':
			oldLines++
			newLines++
		case '-':
			oldLines++
			changed = true
		case '+':
			newLines++
			changed = true
		default:
			return false
		}
		if oldLines > oldCount || newLines > newCount {
			return false
		}
		previousWasBody = true
	}
	return inHunk && oldLines == oldCount && newLines == newCount && changed
}

func parsePullRequestFileHunkHeader(header string) (int, int, bool) {
	if !strings.HasPrefix(header, "@@ -") {
		return 0, 0, false
	}
	oldRange, remainder, found := strings.Cut(strings.TrimPrefix(header, "@@ -"), " +")
	if !found {
		return 0, 0, false
	}
	newRange, suffix, found := strings.Cut(remainder, " @@")
	if !found || suffix != "" && !strings.HasPrefix(suffix, " ") {
		return 0, 0, false
	}
	_, oldCount, oldValid := parsePullRequestFileHunkRange(oldRange)
	_, newCount, newValid := parsePullRequestFileHunkRange(newRange)
	return oldCount, newCount, oldValid && newValid
}

func parsePullRequestFileHunkRange(value string) (int, int, bool) {
	startValue, countValue, hasCount := strings.Cut(value, ",")
	if !validDecimal(startValue) || hasCount && !validDecimal(countValue) {
		return 0, 0, false
	}
	start, err := strconv.Atoi(startValue)
	if err != nil {
		return 0, 0, false
	}
	count := 1
	if hasCount {
		count, err = strconv.Atoi(countValue)
		if err != nil {
			return 0, 0, false
		}
	}
	return start, count, start > 0 || start == 0 && count == 0
}

func validDecimal(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validCheckRunResult(check CheckRun) bool {
	if check.Status != "completed" {
		return check.Conclusion == "" && check.CompletedAt == nil
	}
	if check.CompletedAt == nil {
		return false
	}
	switch check.Conclusion {
	case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
		return true
	default:
		return false
	}
}

func validMarker(marker string) bool {
	if marker == "" {
		return true
	}
	return strings.TrimSpace(marker) == marker && !strings.ContainsAny(marker, "\r\n") && strings.HasPrefix(marker, "<!--") && strings.HasSuffix(marker, "-->") && strings.Count(marker, "<!--") == 1 && strings.Count(marker, "-->") == 1
}

func validReviewCommentRequest(comment ReviewCommentRequest) bool {
	if !validRepositoryPath(comment.Path) || strings.TrimSpace(comment.Body) == "" || comment.Line <= 0 || comment.Side != ReviewSideLeft && comment.Side != ReviewSideRight {
		return false
	}
	if comment.StartLine == 0 {
		return comment.StartSide == ""
	}
	return comment.StartLine > 0 && comment.StartLine < comment.Line && comment.StartSide == comment.Side
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
