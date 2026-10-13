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
	MaxCIDiagnosticTextBytes    = 16 << 10
	MaxCIDiagnosticSummaryBytes = 8 << 10
	MaxCIAnnotationsPerResponse = 20
	MaxCIAnnotationMessageBytes = 4 << 10
	// MaxCIAnnotationFetchBytes bounds a single fetched annotation message.
	// Pages show 4 KiB chunks with continuation; only absurdly large
	// messages end at this documented fetch bound.
	MaxCIAnnotationFetchBytes    = 64 << 10
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
	// Truncated reports that the message or title was clipped to its bound
	// and the remainder is not retained by the provider response.
	Truncated bool `json:"truncated,omitempty"`
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
	var annotationOffset, summaryOffset, textOffset, messageOffset int64
	if cursor != "" {
		decoded, err := decodeCICursor(cursor)
		if err != nil {
			return CheckDiagnostics{}, &ConfigurationError{Cause: err}
		}
		if decoded.Kind != "check_annotations" || decoded.ID != checkID || decoded.RepoID != repoID || decoded.Head != headSHA {
			return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		var perr error
		summaryOffset, textOffset, messageOffset, perr = parseOutputOffsets(decoded.Extra)
		if perr != nil {
			return CheckDiagnostics{}, &ConfigurationError{Cause: perr}
		}
		annotationOffset = decoded.Offset
	}
	check, output, err := client.getCheckRun(ctx, installationToken, owner, repository, checkID)
	if err != nil {
		return CheckDiagnostics{}, err
	}
	if check.HeadSHA != headSHA {
		return CheckDiagnostics{}, responseFailure(responseMetadata{}, "ci_scope_validation", "identity_mismatch", fmt.Errorf("%w: check run head does not match scoped head", ErrInvalidAPIResponse))
	}
	if !validTextOffset(output.Summary, summaryOffset) || !validTextOffset(output.Text, textOffset) {
		return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	summaryChunk, summaryNext, summaryMore := chunkDiagnosticText(output.Summary, summaryOffset, MaxCIDiagnosticSummaryBytes)
	textChunk, textNext, textMore := chunkDiagnosticText(output.Text, textOffset, MaxCIDiagnosticTextBytes)
	title, _ := boundDiagnosticText(output.Title, 1024)
	annotations, err := client.ListCheckRunAnnotations(ctx, installationToken, owner, repository, checkID)
	if err != nil {
		return CheckDiagnostics{}, err
	}
	if annotationOffset > int64(len(annotations)) {
		return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	if annotationOffset < int64(len(annotations)) {
		first := annotations[annotationOffset]
		if messageOffset < 0 || messageOffset > int64(len(first.Message)) || !validTextOffset(first.Message, messageOffset) {
			return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		if messageOffset == int64(len(first.Message)) && len(first.Message) > 0 {
			// Fully consumed remainder: advance past it. Honest cursors
			// always advance past exhausted messages, so this only absorbs
			// a stale position without looping.
			annotationOffset++
			messageOffset = 0
		}
	} else if messageOffset != 0 {
		return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	if cursor != "" && annotationOffset == int64(len(annotations)) && summaryOffset >= int64(len(output.Summary)) && textOffset >= int64(len(output.Text)) {
		// Past every annotation with no output remainder: no honest
		// cursor advances to this state, since issuance requires more
		// content to remain. A first request without a cursor carrying
		// empty content is valid and returns an empty result below.
		return CheckDiagnostics{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	page, nextAnnIdx, nextMsgOff, err := pageCheckAnnotations(check, title, summaryChunk, textChunk, annotations, annotationOffset, messageOffset)
	if err != nil {
		return CheckDiagnostics{}, err
	}
	moreAnn := nextAnnIdx < int64(len(annotations))
	hasMore := moreAnn || summaryMore || textMore
	result := CheckDiagnostics{
		CheckID: check.ID, Name: check.Name, HeadSHA: check.HeadSHA,
		Status: check.Status, Conclusion: check.Conclusion, HTMLURL: check.HTMLURL,
		Output:      CheckOutput{Title: title, Summary: summaryChunk, Text: textChunk},
		Annotations: page, HasMore: hasMore,
		Truncated: summaryMore || textMore || moreAnn || anyAnnotationTruncated(page),
	}
	if hasMore {
		result.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "check_annotations", RepoID: repoID, Head: headSHA, ID: checkID, Offset: nextAnnIdx, Extra: formatOutputOffsets(summaryNext, textNext, nextMsgOff)})
	}
	if err := ensureCISerializedBound(result); err != nil {
		return CheckDiagnostics{}, err
	}
	return result, nil
}

// chunkDiagnosticText returns the bounded chunk of value starting at the
// given byte offset, the offset where the next chunk resumes, and whether
// content remains. Offsets always land on UTF-8 boundaries.
func chunkDiagnosticText(value string, offset int64, limit int) (chunk string, next int64, more bool) {
	if offset < 0 || offset > int64(len(value)) {
		return "", offset, true
	}
	rest := value[offset:]
	if len(rest) <= limit {
		if !utf8.ValidString(rest) {
			return strings.ToValidUTF8(rest, "�"), int64(len(value)), false
		}
		return rest, int64(len(value)), false
	}
	cut := rest[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, offset + int64(len(cut)), true
}

// validTextOffset reports whether offset splits value only at UTF-8
// boundaries, so continuation never hides or duplicates bytes.
func validTextOffset(value string, offset int64) bool {
	if offset < 0 || offset > int64(len(value)) {
		return false
	}
	return utf8.ValidString(value[:offset]) && utf8.ValidString(value[offset:])
}

// parseOutputOffsets decodes the check-diagnostics cursor suffix carrying
// summary, text, and message resume offsets. An empty suffix is the first
// page; anything else must match the canonical encoding exactly.
func parseOutputOffsets(extra string) (summaryOff, textOff, msgOff int64, err error) {
	if extra == "" {
		return 0, 0, 0, nil
	}
	var s, t, m int64
	n, scanErr := fmt.Sscanf(extra, "s%d/t%d/m%d", &s, &t, &m)
	if scanErr != nil || n != 3 || s < 0 || t < 0 || m < 0 || extra != formatOutputOffsets(s, t, m) {
		return 0, 0, 0, fmt.Errorf("%w: stale continuation", ErrInvalidAPIResponse)
	}
	return s, t, m, nil
}

func formatOutputOffsets(summaryOff, textOff, msgOff int64) string {
	return fmt.Sprintf("s%d/t%d/m%d", summaryOff, textOff, msgOff)
}

// pageCheckAnnotations fits annotation chunks into one response page within
// both the item-count cap and the serialized budget. The first annotation
// resumes at its message offset; every annotation shows a bounded message
// chunk. It returns the page and the cursor position for the remainder.
func pageCheckAnnotations(check CheckRun, title, summaryChunk, textChunk string, annotations []CheckAnnotation, annotationOffset, messageOffset int64) (page []CheckAnnotation, nextAnnIdx, nextMsgOff int64, err error) {
	page = make([]CheckAnnotation, 0, MaxCIAnnotationsPerResponse)
	nextAnnIdx = annotationOffset
	if annotationOffset >= int64(len(annotations)) {
		return page, nextAnnIdx, 0, nil
	}
	for idx := annotationOffset; idx < int64(len(annotations)) && int64(len(page)) < MaxCIAnnotationsPerResponse; idx++ {
		item := annotations[idx]
		var off int64
		if idx == annotationOffset {
			off = messageOffset
		}
		chunk, chunkNext, chunkMore := chunkDiagnosticText(item.Message, off, MaxCIAnnotationMessageBytes)
		trial := item
		trial.Message = chunk
		trial.Truncated = item.Truncated || chunkMore
		if !checkDiagnosticsPageFits(check, title, summaryChunk, textChunk, append(page, trial)) {
			if len(page) == 0 {
				// A single annotation with its output chunks exceeds the
				// budget even at one message chunk: report resource
				// exhaustion explicitly rather than looping on an empty
				// page or emitting an oversized response. Output chunks
				// and one message chunk always fit in practice.
				return nil, 0, 0, &ConfigurationError{Cause: fmt.Errorf("%w: single annotation exceeds size limit", ErrInvalidAPIResponse)}
			}
			// Doesn't fit: resume here (or within this message).
			return page, idx, off, nil
		}
		page = append(page, trial)
		if chunkMore {
			// Message continues: the page ends here so the remainder is
			// continued exactly once on the next page.
			return page, idx, chunkNext, nil
		}
		nextAnnIdx = idx + 1
	}
	return page, nextAnnIdx, 0, nil
}

// checkDiagnosticsPageFits probes whether a diagnostics page fits the
// serialized response budget. It assumes a continuation follows (with room
// for its cursor), so an accepted page always fits once finalized.
func checkDiagnosticsPageFits(check CheckRun, title, summaryChunk, textChunk string, page []CheckAnnotation) bool {
	probe := CheckDiagnostics{
		CheckID: check.ID, Name: check.Name, HeadSHA: check.HeadSHA,
		Status: check.Status, Conclusion: check.Conclusion, HTMLURL: check.HTMLURL,
		Output:      CheckOutput{Title: title, Summary: summaryChunk, Text: textChunk},
		Annotations: page, HasMore: true, NextCursor: strings.Repeat("x", maxEncodedCursorLength), Truncated: true,
	}
	return ensureCISerializedBound(probe) == nil
}

func anyAnnotationTruncated(page []CheckAnnotation) bool {
	for _, annotation := range page {
		if annotation.Truncated {
			return true
		}
	}
	return false
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
			// Fetch messages up to a bound far above the 4 KiB page
			// chunk so tails remain retrievable through continuation.
			// The flag records clipping; page assembly reports it.
			message, messageClipped := boundDiagnosticText(*raw.Message, MaxCIAnnotationFetchBytes)
			var title string
			var titleClipped bool
			if raw.Title != nil {
				title, titleClipped = boundDiagnosticText(*raw.Title, 1024)
			}
			key := fmt.Sprintf("%s:%d:%d:%s:%s", raw.Path, *raw.StartLine, *raw.EndLine, raw.AnnotationLevel, *raw.Message)
			if _, exists := seen[key]; exists {
				return nil, fmt.Errorf("%w: duplicate check annotation", ErrInvalidAPIResponse)
			}
			seen[key] = struct{}{}
			annotations = append(annotations, CheckAnnotation{
				Path: raw.Path, StartLine: *raw.StartLine, EndLine: *raw.EndLine,
				AnnotationLevel: raw.AnnotationLevel, Message: message, Title: title,
				Truncated: messageClipped || titleClipped,
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
		// A defensive guard: page builders above keep every response
		// within budget, so reaching here maps to resource exhaustion
		// rather than an unavailable diagnostic.
		return &ConfigurationError{Cause: fmt.Errorf("%w: diagnostics response exceeds size limit", ErrInvalidAPIResponse)}
	}
	return nil
}
