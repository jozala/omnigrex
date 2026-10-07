package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxCIDiagnosticTextBytes     = 16 << 10
	MaxCIDiagnosticSummaryBytes  = 8 << 10
	MaxCIAnnotationsPerResponse  = 20
	MaxCIAnnotationMessageBytes  = 4 << 10
	MaxCIRunsPerResponse         = 20
	MaxCIJobsPerResponse         = 20
	MaxCILogExcerptBytes         = 32 << 10
	DefaultCILogExcerptBytes     = 16 << 10
	MinCILogExcerptBytes         = 1024
	MaxCILogScanBytes            = 8 << 10 << 10
	MaxCISearchMatches           = 20
	MaxCISearchContext           = 20
	MaxCIQueryLength             = 256
	MaxCILogDownloadBytes        = 10 << 10 << 10
	MaxCISerializedResponseBytes = 64 << 10
)

type CheckOutput struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Text    string `json:"text,omitempty"`
}

type CheckAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
	Title           string `json:"title,omitempty"`
}

type CheckDiagnostics struct {
	CheckID     int64             `json:"check_run_id"`
	Name        string            `json:"name"`
	HeadSHA     string            `json:"head_sha"`
	Status      string            `json:"status"`
	Conclusion  string            `json:"conclusion,omitempty"`
	HTMLURL     string            `json:"html_url"`
	Output      CheckOutput       `json:"output"`
	Annotations []CheckAnnotation `json:"annotations"`
	HasMore     bool              `json:"has_more"`
	NextCursor  string            `json:"next_cursor,omitempty"`
	Truncated   bool              `json:"truncated"`
}

type CIRun struct {
	ID         int64      `json:"run_id"`
	Name       string     `json:"name,omitempty"`
	HeadSHA    string     `json:"head_sha"`
	HeadBranch string     `json:"head_branch,omitempty"`
	Status     string     `json:"status"`
	Conclusion string     `json:"conclusion,omitempty"`
	HTMLURL    string     `json:"html_url"`
	RunNumber  int64      `json:"run_number"`
	RunAttempt int        `json:"run_attempt,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

type CIRunList struct {
	HeadSHA    string  `json:"head_sha"`
	Runs       []CIRun `json:"runs"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type CIStep struct {
	Name        string     `json:"name"`
	Number      int        `json:"number"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type CIJob struct {
	ID          int64      `json:"job_id"`
	RunID       int64      `json:"run_id"`
	RunAttempt  int        `json:"run_attempt"`
	HeadSHA     string     `json:"head_sha"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion,omitempty"`
	HTMLURL     string     `json:"html_url"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Steps       []CIStep   `json:"steps"`
}

type CIRunDetail struct {
	Run        CIRun   `json:"run"`
	Jobs       []CIJob `json:"jobs"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type CILogExcerpt struct {
	HeadSHA     string `json:"head_sha"`
	RunID       int64  `json:"run_id,omitempty"`
	RunAttempt  int    `json:"run_attempt,omitempty"`
	JobID       int64  `json:"job_id"`
	StepNumber  *int   `json:"step_number,omitempty"`
	Text        string `json:"text"`
	StartOffset int64  `json:"start_offset"`
	EndOffset   int64  `json:"end_offset"`
	HasMore     bool   `json:"has_more"`
	NextCursor  string `json:"next_cursor,omitempty"`
	Truncated   bool   `json:"truncated"`
	Empty       bool   `json:"empty,omitempty"`
}

type CILogMatch struct {
	LineNumber    int    `json:"line_number"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	StartOffset   int64  `json:"start_offset"`
	EndOffset     int64  `json:"end_offset"`
	Text          string `json:"text"`
	TruncatedLine bool   `json:"truncated_line,omitempty"`
}

type CILogSearchResult struct {
	HeadSHA       string       `json:"head_sha"`
	JobID         int64        `json:"job_id"`
	Query         string       `json:"query"`
	Matches       []CILogMatch `json:"matches"`
	HasMore       bool         `json:"has_more"`
	NextCursor    string       `json:"next_cursor,omitempty"`
	ReachedEnd    bool         `json:"reached_end"`
	SearchedBytes int64        `json:"searched_bytes"`
}

type JobScope struct {
	ID         int64
	RunID      int64
	RunAttempt int
	HeadSHA    string
}

type ciCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	RepoID  int64  `json:"repo"`
	Head    string `json:"head"`
	ID      int64  `json:"id"`
	Attempt int    `json:"attempt,omitempty"`
	Offset  int64  `json:"offset"`
	Extra   string `json:"extra,omitempty"`
}

func encodeCICursor(cursor ciCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeCICursor(raw string) (ciCursor, error) {
	var cursor ciCursor
	if raw == "" || len(raw) > 4096 {
		return ciCursor{}, fmt.Errorf("%w: continuation is invalid", ErrInvalidAPIResponse)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 || len(decoded) > 2048 {
		return ciCursor{}, fmt.Errorf("%w: continuation is invalid", ErrInvalidAPIResponse)
	}
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return ciCursor{}, fmt.Errorf("%w: continuation is invalid", ErrInvalidAPIResponse)
	}
	if cursor.Version != 1 || cursor.Kind == "" || cursor.RepoID <= 0 || !validCommitSHA(cursor.Head) || cursor.ID < 0 || cursor.Offset < 0 {
		return ciCursor{}, fmt.Errorf("%w: continuation is invalid", ErrInvalidAPIResponse)
	}
	return cursor, nil
}

func boundDiagnosticText(value string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len(value) != 0
	}
	if len(value) <= limit {
		if !utf8.ValidString(value) {
			return strings.ToValidUTF8(value, "�"), true
		}
		return value, false
	}
	truncated := value[:limit]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated, true
}

func (client *APIClient) GetCheckRunDiagnostics(ctx context.Context, installationToken, owner, repository string, checkID int64, cursor string, repoID int64, headSHA string) (CheckDiagnostics, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CheckDiagnostics{}, err
	}
	if checkID <= 0 {
		return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("invalid check run id")}
	}
	if !validCommitSHA(headSHA) {
		return CheckDiagnostics{}, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}
	var offset int64
	if cursor != "" {
		decoded, err := decodeCICursor(cursor)
		if err != nil {
			return CheckDiagnostics{}, &ConfigurationError{Cause: err}
		}
		if decoded.Kind != "check_annotations" || decoded.ID != checkID || decoded.RepoID != repoID || decoded.Head != headSHA {
			return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		offset = decoded.Offset
	}
	check, output, err := client.getCheckRun(ctx, installationToken, owner, repository, checkID)
	if err != nil {
		return CheckDiagnostics{}, err
	}
	if check.HeadSHA != headSHA {
		return CheckDiagnostics{}, responseFailure(responseMetadata{}, "ci_scope_validation", "identity_mismatch", fmt.Errorf("%w: check run head does not match scoped head", ErrInvalidAPIResponse))
	}
	summary, summaryTruncated := boundDiagnosticText(output.Summary, MaxCIDiagnosticSummaryBytes)
	text, textTruncated := boundDiagnosticText(output.Text, MaxCIDiagnosticTextBytes)
	title, _ := boundDiagnosticText(output.Title, 1024)
	annotations, hasMore, nextOffset, err := client.listCheckRunAnnotationsPage(ctx, installationToken, owner, repository, checkID, offset)
	if err != nil {
		return CheckDiagnostics{}, err
	}
	result := CheckDiagnostics{
		CheckID: check.ID, Name: check.Name, HeadSHA: check.HeadSHA,
		Status: check.Status, Conclusion: check.Conclusion, HTMLURL: check.HTMLURL,
		Output:      CheckOutput{Title: title, Summary: summary, Text: text},
		Annotations: annotations, HasMore: hasMore, Truncated: summaryTruncated || textTruncated || hasMore,
	}
	if hasMore {
		result.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "check_annotations", RepoID: repoID, Head: headSHA, ID: checkID, Offset: nextOffset})
	}
	if err := ensureCISerializedBound(result); err != nil {
		return CheckDiagnostics{}, err
	}
	return result, nil
}

type rawCheckRun struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion"`
	HTMLURL     string     `json:"html_url"`
	DetailsURL  string     `json:"details_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Output      *struct {
		Title   *string `json:"title"`
		Summary *string `json:"summary"`
		Text    *string `json:"text"`
	} `json:"output"`
}

func (client *APIClient) getCheckRun(ctx context.Context, installationToken, owner, repository string, checkID int64) (CheckRun, CheckOutput, error) {
	path := fmt.Sprintf("/repos/%s/%s/check-runs/%d", url.PathEscape(owner), url.PathEscape(repository), checkID)
	var raw rawCheckRun
	metadata, err := client.doJSONWithMetadata(ctx, http.MethodGet, path, installationToken, nil, &raw)
	if err != nil {
		return CheckRun{}, CheckOutput{}, err
	}
	if raw.ID != checkID || raw.NodeID == "" || strings.TrimSpace(raw.Name) == "" || !validCommitSHA(raw.HeadSHA) || !validCheckRunStatus(raw.Status) {
		return CheckRun{}, CheckOutput{}, responseFailure(metadata, "ci_check_validation", "invalid_commit", ErrInvalidAPIResponse)
	}
	conclusion := ""
	if raw.Conclusion != nil {
		conclusion = *raw.Conclusion
	}
	check := CheckRun{ID: raw.ID, NodeID: raw.NodeID, Name: raw.Name, HeadSHA: raw.HeadSHA, Status: raw.Status, Conclusion: conclusion, HTMLURL: raw.HTMLURL, DetailsURL: raw.DetailsURL, StartedAt: raw.StartedAt, CompletedAt: raw.CompletedAt}
	if !validCheckRunResult(check) || !validResponseURL(check.HTMLURL) || check.DetailsURL != "" && !validResponseURL(check.DetailsURL) {
		return CheckRun{}, CheckOutput{}, responseFailure(metadata, "ci_check_validation", "invalid_state", ErrInvalidAPIResponse)
	}
	output := CheckOutput{}
	if raw.Output != nil {
		if raw.Output.Title != nil {
			output.Title = *raw.Output.Title
		}
		if raw.Output.Summary != nil {
			output.Summary = *raw.Output.Summary
		}
		if raw.Output.Text != nil {
			output.Text = *raw.Output.Text
		}
	}
	return check, output, nil
}

func (client *APIClient) listCheckRunAnnotationsPage(ctx context.Context, installationToken, owner, repository string, checkID, offset int64) ([]CheckAnnotation, bool, int64, error) {
	annotations, err := client.ListCheckRunAnnotations(ctx, installationToken, owner, repository, checkID)
	if err != nil {
		return nil, false, 0, err
	}
	if offset > int64(len(annotations)) {
		return nil, false, 0, fmt.Errorf("%w: stale continuation", ErrInvalidAPIResponse)
	}
	end := offset + MaxCIAnnotationsPerResponse
	if end < int64(len(annotations)) {
		return annotations[offset:end], true, end, nil
	}
	return annotations[offset:], false, 0, nil
}

func (client *APIClient) ListCheckRunAnnotations(ctx context.Context, installationToken, owner, repository string, checkID int64) ([]CheckAnnotation, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if checkID <= 0 {
		return nil, &ConfigurationError{Cause: fmt.Errorf("invalid check run id")}
	}
	path := fmt.Sprintf("/repos/%s/%s/check-runs/%d/annotations?per_page=100", url.PathEscape(owner), url.PathEscape(repository), checkID)
	annotations := make([]CheckAnnotation, 0)
	seen := make(map[string]struct{})
	for path != "" {
		var page []struct {
			Path            string  `json:"path"`
			StartLine       *int    `json:"start_line"`
			EndLine         *int    `json:"end_line"`
			AnnotationLevel string  `json:"annotation_level"`
			Message         *string `json:"message"`
			Title           *string `json:"title"`
		}
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, installationToken, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, raw := range page {
			if !validRepositoryPath(raw.Path) || raw.StartLine == nil || *raw.StartLine <= 0 || raw.EndLine == nil || *raw.EndLine < *raw.StartLine || raw.Message == nil {
				return nil, fmt.Errorf("%w: check annotation is invalid", ErrInvalidAPIResponse)
			}
			switch raw.AnnotationLevel {
			case "notice", "warning", "failure":
			default:
				return nil, fmt.Errorf("%w: check annotation level is invalid", ErrInvalidAPIResponse)
			}
			message, _ := boundDiagnosticText(*raw.Message, MaxCIAnnotationMessageBytes)
			title := ""
			if raw.Title != nil {
				title, _ = boundDiagnosticText(*raw.Title, 1024)
			}
			key := fmt.Sprintf("%s:%d:%d:%s:%s", raw.Path, *raw.StartLine, *raw.EndLine, raw.AnnotationLevel, message)
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("%w: duplicate check annotation", ErrInvalidAPIResponse)
			}
			seen[key] = struct{}{}
			annotations = append(annotations, CheckAnnotation{
				Path: raw.Path, StartLine: *raw.StartLine, EndLine: *raw.EndLine,
				AnnotationLevel: raw.AnnotationLevel, Message: message, Title: title,
			})
			if len(annotations) > 1000 {
				return nil, fmt.Errorf("%w: too many check annotations", ErrInvalidAPIResponse)
			}
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		nextPath, paginationErr := client.paginationPath(next)
		if paginationErr != nil {
			return nil, paginationErr
		}
		path = nextPath
	}
	return annotations, nil
}

type rawWorkflowRun struct {
	ID         int64      `json:"id"`
	Name       *string    `json:"name"`
	HeadBranch *string    `json:"head_branch"`
	HeadSHA    string     `json:"head_sha"`
	Status     *string    `json:"status"`
	Conclusion *string    `json:"conclusion"`
	HTMLURL    string     `json:"html_url"`
	RunNumber  int64      `json:"run_number"`
	RunAttempt *int       `json:"run_attempt"`
	CreatedAt  *time.Time `json:"created_at"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

func rawRunToCIRun(raw rawWorkflowRun) (CIRun, error) {
	if raw.ID <= 0 || !validCommitSHA(raw.HeadSHA) || !validResponseURL(raw.HTMLURL) || raw.RunNumber <= 0 {
		return CIRun{}, fmt.Errorf("%w: workflow run is invalid", ErrInvalidAPIResponse)
	}
	status := ""
	if raw.Status != nil {
		status = *raw.Status
	}
	if status == "" {
		if raw.Conclusion != nil && *raw.Conclusion != "" {
			status = "completed"
		} else {
			return CIRun{}, fmt.Errorf("%w: workflow run status is missing", ErrInvalidAPIResponse)
		}
	}
	switch status {
	case "queued", "in_progress", "completed", "waiting", "requested", "pending":
	default:
		return CIRun{}, fmt.Errorf("%w: workflow run status is invalid", ErrInvalidAPIResponse)
	}
	conclusion := ""
	if raw.Conclusion != nil {
		conclusion = *raw.Conclusion
	}
	if status == "completed" {
		switch conclusion {
		case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
		default:
			return CIRun{}, fmt.Errorf("%w: workflow run conclusion is invalid", ErrInvalidAPIResponse)
		}
	} else if conclusion != "" {
		return CIRun{}, fmt.Errorf("%w: incomplete run has conclusion", ErrInvalidAPIResponse)
	}
	name := ""
	if raw.Name != nil {
		name = *raw.Name
	}
	branch := ""
	if raw.HeadBranch != nil {
		branch = *raw.HeadBranch
	}
	attempt := 0
	if raw.RunAttempt != nil {
		attempt = *raw.RunAttempt
	}
	return CIRun{
		ID: raw.ID, Name: name, HeadSHA: raw.HeadSHA, HeadBranch: branch,
		Status: status, Conclusion: conclusion, HTMLURL: raw.HTMLURL,
		RunNumber: raw.RunNumber, RunAttempt: attempt,
		CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt,
	}, nil
}

func (client *APIClient) ListWorkflowRunsForHead(ctx context.Context, installationToken, owner, repository, headSHA, cursor string, repoID int64) (CIRunList, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CIRunList{}, err
	}
	if !validCommitSHA(headSHA) {
		return CIRunList{}, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}
	var offset int64
	if cursor != "" {
		decoded, err := decodeCICursor(cursor)
		if err != nil {
			return CIRunList{}, &ConfigurationError{Cause: err}
		}
		if decoded.Kind != "ci_runs" || decoded.RepoID != repoID || decoded.Head != headSHA {
			return CIRunList{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		offset = decoded.Offset
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=100", url.PathEscape(owner), url.PathEscape(repository), headSHA)
	runs := make([]CIRun, 0)
	seen := make(map[int64]struct{})
	total := -1
	for path != "" {
		var page struct {
			TotalCount   *int             `json:"total_count"`
			WorkflowRuns []rawWorkflowRun `json:"workflow_runs"`
		}
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, installationToken, nil, &page)
		if err != nil {
			return CIRunList{}, err
		}
		if page.TotalCount == nil || *page.TotalCount < 0 {
			return CIRunList{}, fmt.Errorf("%w: workflow run count is invalid", ErrInvalidAPIResponse)
		}
		if total < 0 {
			total = *page.TotalCount
		} else if *page.TotalCount != total {
			return CIRunList{}, fmt.Errorf("%w: workflow run count changed", ErrInvalidAPIResponse)
		}
		for _, raw := range page.WorkflowRuns {
			run, err := rawRunToCIRun(raw)
			if err != nil {
				return CIRunList{}, err
			}
			if run.HeadSHA != headSHA {
				return CIRunList{}, fmt.Errorf("%w: workflow run head does not match scoped head", ErrInvalidAPIResponse)
			}
			if _, exists := seen[run.ID]; exists {
				return CIRunList{}, fmt.Errorf("%w: duplicate workflow run id", ErrInvalidAPIResponse)
			}
			seen[run.ID] = struct{}{}
			runs = append(runs, run)
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		nextPath, paginationErr := client.paginationPath(next)
		if paginationErr != nil {
			return CIRunList{}, paginationErr
		}
		path = nextPath
	}
	if offset > int64(len(runs)) {
		return CIRunList{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	end := offset + MaxCIRunsPerResponse
	hasMore := end < int64(len(runs))
	var window []CIRun
	if hasMore {
		window = runs[offset:end]
	} else {
		window = runs[offset:]
	}
	if window == nil {
		window = []CIRun{}
	}
	result := CIRunList{HeadSHA: headSHA, Runs: window, HasMore: hasMore}
	if hasMore {
		result.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "ci_runs", RepoID: repoID, Head: headSHA, Offset: end})
	}
	if err := ensureCISerializedBound(result); err != nil {
		return CIRunList{}, err
	}
	return result, nil
}

func (client *APIClient) GetWorkflowRun(ctx context.Context, installationToken, owner, repository string, runID int64) (CIRun, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CIRun{}, err
	}
	if runID <= 0 {
		return CIRun{}, &ConfigurationError{Cause: fmt.Errorf("invalid run id")}
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(repository), runID)
	var raw rawWorkflowRun
	metadata, err := client.doJSONWithMetadata(ctx, http.MethodGet, path, installationToken, nil, &raw)
	if err != nil {
		return CIRun{}, err
	}
	if raw.ID != runID {
		return CIRun{}, responseFailure(metadata, "ci_run_validation", "identity_mismatch", ErrInvalidAPIResponse)
	}
	run, err := rawRunToCIRun(raw)
	if err != nil {
		return CIRun{}, responseFailure(metadata, "ci_run_validation", "", err)
	}
	return run, nil
}

type rawCIJob struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	RunAttempt  *int       `json:"run_attempt"`
	HeadSHA     *string    `json:"head_sha"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion"`
	HTMLURL     string     `json:"html_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Steps       []struct {
		Name        string     `json:"name"`
		Number      int        `json:"number"`
		Status      string     `json:"status"`
		Conclusion  *string    `json:"conclusion"`
		StartedAt   *time.Time `json:"started_at"`
		CompletedAt *time.Time `json:"completed_at"`
	} `json:"steps"`
}

func rawJobToCIJob(raw rawCIJob) (CIJob, error) {
	if raw.ID <= 0 || raw.RunID <= 0 || strings.TrimSpace(raw.Name) == "" || !validResponseURL(raw.HTMLURL) {
		return CIJob{}, fmt.Errorf("%w: job is invalid", ErrInvalidAPIResponse)
	}
	attempt := 0
	if raw.RunAttempt != nil {
		attempt = *raw.RunAttempt
	}
	head := ""
	if raw.HeadSHA != nil {
		head = *raw.HeadSHA
	}
	switch raw.Status {
	case "queued", "in_progress", "completed", "waiting", "requested", "pending":
	default:
		return CIJob{}, fmt.Errorf("%w: job status is invalid", ErrInvalidAPIResponse)
	}
	conclusion := ""
	if raw.Conclusion != nil {
		conclusion = *raw.Conclusion
	}
	if raw.Status == "completed" {
		switch conclusion {
		case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
		default:
			return CIJob{}, fmt.Errorf("%w: job conclusion is invalid", ErrInvalidAPIResponse)
		}
		if raw.CompletedAt == nil {
			return CIJob{}, fmt.Errorf("%w: completed job is missing completion time", ErrInvalidAPIResponse)
		}
	} else if conclusion != "" {
		return CIJob{}, fmt.Errorf("%w: incomplete job has conclusion", ErrInvalidAPIResponse)
	}
	steps := make([]CIStep, 0, len(raw.Steps))
	seenNumbers := make(map[int]struct{})
	for _, step := range raw.Steps {
		if strings.TrimSpace(step.Name) == "" || step.Number <= 0 {
			return CIJob{}, fmt.Errorf("%w: job step is invalid", ErrInvalidAPIResponse)
		}
		if _, exists := seenNumbers[step.Number]; exists {
			return CIJob{}, fmt.Errorf("%w: duplicate job step number", ErrInvalidAPIResponse)
		}
		seenNumbers[step.Number] = struct{}{}
		switch step.Status {
		case "queued", "in_progress", "completed", "waiting", "requested", "pending":
		default:
			return CIJob{}, fmt.Errorf("%w: job step status is invalid", ErrInvalidAPIResponse)
		}
		stepConclusion := ""
		if step.Conclusion != nil {
			stepConclusion = *step.Conclusion
		}
		if step.Status == "completed" {
			switch stepConclusion {
			case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
			default:
				return CIJob{}, fmt.Errorf("%w: job step conclusion is invalid", ErrInvalidAPIResponse)
			}
		} else if stepConclusion != "" {
			return CIJob{}, fmt.Errorf("%w: incomplete step has conclusion", ErrInvalidAPIResponse)
		}
		steps = append(steps, CIStep{
			Name: step.Name, Number: step.Number, Status: step.Status, Conclusion: stepConclusion,
			StartedAt: step.StartedAt, CompletedAt: step.CompletedAt,
		})
	}
	return CIJob{
		ID: raw.ID, RunID: raw.RunID, RunAttempt: attempt, HeadSHA: head,
		Name: raw.Name, Status: raw.Status, Conclusion: conclusion, HTMLURL: raw.HTMLURL,
		StartedAt: raw.StartedAt, CompletedAt: raw.CompletedAt, Steps: steps,
	}, nil
}

func (client *APIClient) GetWorkflowRunDetail(ctx context.Context, installationToken, owner, repository string, runID int64, attempt int, cursor string, repoID int64, headSHA string) (CIRunDetail, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CIRunDetail{}, err
	}
	if runID <= 0 {
		return CIRunDetail{}, &ConfigurationError{Cause: fmt.Errorf("invalid run id")}
	}
	if attempt < 0 || attempt > 1000 {
		return CIRunDetail{}, &ConfigurationError{Cause: fmt.Errorf("invalid attempt")}
	}
	if !validCommitSHA(headSHA) {
		return CIRunDetail{}, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}
	var offset int64
	if cursor != "" {
		decoded, err := decodeCICursor(cursor)
		if err != nil {
			return CIRunDetail{}, &ConfigurationError{Cause: err}
		}
		if decoded.Kind != "ci_jobs" || decoded.ID != runID || decoded.RepoID != repoID || decoded.Head != headSHA || decoded.Attempt != attempt {
			return CIRunDetail{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		offset = decoded.Offset
	}
	run, err := client.GetWorkflowRun(ctx, installationToken, owner, repository, runID)
	if err != nil {
		return CIRunDetail{}, err
	}
	if run.HeadSHA != headSHA {
		return CIRunDetail{}, fmt.Errorf("%w: workflow run head does not match scoped head", ErrInvalidAPIResponse)
	}
	jobs, err := client.listJobsForRun(ctx, installationToken, owner, repository, runID, attempt)
	if err != nil {
		return CIRunDetail{}, err
	}
	for _, job := range jobs {
		if job.RunID != runID {
			return CIRunDetail{}, fmt.Errorf("%w: job run identity mismatch", ErrInvalidAPIResponse)
		}
		if attempt != 0 && job.RunAttempt != attempt {
			return CIRunDetail{}, fmt.Errorf("%w: job attempt mismatch", ErrInvalidAPIResponse)
		}
		if job.HeadSHA != "" && job.HeadSHA != headSHA {
			return CIRunDetail{}, fmt.Errorf("%w: job head does not match scoped head", ErrInvalidAPIResponse)
		}
	}
	if offset > int64(len(jobs)) {
		return CIRunDetail{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	end := offset + MaxCIJobsPerResponse
	hasMore := end < int64(len(jobs))
	var window []CIJob
	if hasMore {
		window = jobs[offset:end]
	} else {
		window = jobs[offset:]
		if window == nil {
			window = []CIJob{}
		}
	}
	result := CIRunDetail{Run: run, Jobs: window, HasMore: hasMore}
	if hasMore {
		result.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "ci_jobs", RepoID: repoID, Head: headSHA, ID: runID, Attempt: attempt, Offset: end})
	}
	if err := ensureCISerializedBound(result); err != nil {
		return CIRunDetail{}, err
	}
	return result, nil
}

func (client *APIClient) listJobsForRun(ctx context.Context, installationToken, owner, repository string, runID int64, attempt int) ([]CIJob, error) {
	var path string
	if attempt == 0 {
		path = fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=100", url.PathEscape(owner), url.PathEscape(repository), runID)
	} else {
		path = fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", url.PathEscape(owner), url.PathEscape(repository), runID, attempt)
	}
	jobs := make([]CIJob, 0)
	seen := make(map[int64]struct{})
	for path != "" {
		var page struct {
			TotalCount *int       `json:"total_count"`
			Jobs       []rawCIJob `json:"jobs"`
		}
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, installationToken, nil, &page)
		if err != nil {
			return nil, err
		}
		if page.TotalCount == nil || *page.TotalCount < len(page.Jobs) {
			return nil, fmt.Errorf("%w: job count is invalid", ErrInvalidAPIResponse)
		}
		for _, raw := range page.Jobs {
			job, err := rawJobToCIJob(raw)
			if err != nil {
				return nil, err
			}
			if _, exists := seen[job.ID]; exists {
				return nil, fmt.Errorf("%w: duplicate job id", ErrInvalidAPIResponse)
			}
			seen[job.ID] = struct{}{}
			jobs = append(jobs, job)
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		nextPath, paginationErr := client.paginationPath(next)
		if paginationErr != nil {
			return nil, paginationErr
		}
		path = nextPath
	}
	return jobs, nil
}

func (client *APIClient) GetCIJob(ctx context.Context, installationToken, owner, repository string, jobID int64) (CIJob, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CIJob{}, err
	}
	if jobID <= 0 {
		return CIJob{}, &ConfigurationError{Cause: fmt.Errorf("invalid job id")}
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d", url.PathEscape(owner), url.PathEscape(repository), jobID)
	var raw rawCIJob
	metadata, err := client.doJSONWithMetadata(ctx, http.MethodGet, path, installationToken, nil, &raw)
	if err != nil {
		return CIJob{}, err
	}
	if raw.ID != jobID {
		return CIJob{}, responseFailure(metadata, "ci_job_validation", "identity_mismatch", ErrInvalidAPIResponse)
	}
	job, err := rawJobToCIJob(raw)
	if err != nil {
		return CIJob{}, responseFailure(metadata, "ci_job_validation", "", err)
	}
	return job, nil
}

func ensureCISerializedBound(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: diagnostics encoding failed", ErrInvalidAPIResponse)
	}
	if len(encoded) > MaxCISerializedResponseBytes {
		return fmt.Errorf("%w: diagnostics response exceeds size limit", ErrInvalidAPIResponse)
	}
	return nil
}
