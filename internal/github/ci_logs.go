package github

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

var logDownloadClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (client *APIClient) jobLogsLocation(ctx context.Context, installationToken, owner, repository string, jobID int64) (string, responseMetadata, error) {
	if err := validateRepository(owner, repository); err != nil {
		return "", responseMetadata{}, err
	}
	if installationToken == "" {
		return "", responseMetadata{}, &ConfigurationError{Cause: ErrMissingCredential}
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", url.PathEscape(owner), url.PathEscape(repository), jobID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+path, nil)
	if err != nil {
		return "", responseMetadata{}, fmt.Errorf("create job logs request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", APIVersion)
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("Authorization", "Bearer "+installationToken)
	redirectClient := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if doer, ok := client.httpClient.(*http.Client); ok && doer != nil && doer.Timeout != 0 {
		redirectClient.Timeout = doer.Timeout
	}
	response, err := redirectClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", responseMetadata{}, responseFailure(responseMetadata{}, "github_http", CancellationReason(ctx.Err()), ctx.Err())
		}
		reason := CancellationReason(err)
		if reason == "" {
			reason = "transport_failed"
		}
		return "", responseMetadata{}, responseFailure(responseMetadata{}, "github_http", reason, &TransientError{Cause: err})
	}
	defer response.Body.Close()
	metadata := responseMetadata{header: response.Header.Clone(), status: response.StatusCode}
	switch response.StatusCode {
	case http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		location := response.Header.Get("Location")
		if location == "" {
			return "", metadata, responseFailure(metadata, "ci_logs_location", "missing_data", ErrInvalidAPIResponse)
		}
		return location, metadata, nil
	case http.StatusNotFound, http.StatusGone:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return "", metadata, responseFailure(metadata, "ci_logs_location", "http_rejected", classifyAPIError(response, http.MethodGet, path))
	default:
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
			return "", metadata, responseFailure(metadata, "github_http", "http_rejected", classifyAPIError(response, http.MethodGet, path))
		}
		return "", metadata, responseFailure(metadata, "ci_logs_location", "invalid_json", ErrInvalidAPIResponse)
	}
}

func validateLogDownloadURL(rawLocation, apiBaseURL string) (string, error) {
	if rawLocation == "" {
		return "", fmt.Errorf("log download location is missing")
	}
	if strings.Contains(rawLocation, "@") {
		return "", fmt.Errorf("log download location has credentials")
	}
	parsed, err := url.Parse(rawLocation)
	if err != nil || parsed == nil || parsed.Host == "" {
		return "", fmt.Errorf("log download location is invalid")
	}
	apiParsed, _ := url.Parse(apiBaseURL)
	apiLoopback := apiParsed != nil && isLoopbackHost(apiParsed.Hostname())
	if apiLoopback && isLoopbackHost(parsed.Hostname()) {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", fmt.Errorf("log download location must use HTTPS")
		}
		if parsed.User != nil {
			return "", fmt.Errorf("log download location has credentials")
		}
		return parsed.String(), nil
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("log download location must use HTTPS")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("log download location has credentials")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", fmt.Errorf("log download location is invalid")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return "", fmt.Errorf("log download location has unexpected port")
	}
	if isLoopbackHost(host) || isPrivateHost(host) {
		return "", fmt.Errorf("log download location is not an approved destination")
	}
	lowerHost := strings.ToLower(host)
	for _, suffix := range []string{".blob.core.windows.net", ".actions.githubusercontent.com", "actions.githubusercontent.com", "objects.githubusercontent.com", "githubusercontent.com"} {
		if strings.HasSuffix(lowerHost, suffix) || lowerHost == strings.TrimPrefix(suffix, ".") {
			return parsed.String(), nil
		}
	}
	return "", fmt.Errorf("log download location is not an approved destination")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func isPrivateHost(host string) bool {
	address := net.ParseIP(host)
	if address == nil {
		return false
	}
	return address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsUnspecified()
}

func fetchLogBytes(ctx context.Context, downloadURL string, offset int64, maxBytes int64) ([]byte, bool, error) {
	if offset < 0 || maxBytes <= 0 {
		return nil, false, fmt.Errorf("invalid log range")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("User-Agent", UserAgent)
	request.Header.Set("Accept", "text/plain")
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := logDownloadClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, &TransientError{Cause: err}
	}
	defer response.Body.Close()
	if offset > 0 && response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return []byte{}, true, nil
	}
	if offset > 0 && response.StatusCode == http.StatusOK {
		discarded, err := discardLogPrefix(ctx, response.Body, offset)
		if err != nil {
			return nil, false, err
		}
		if !discarded {
			return []byte{}, true, nil
		}
		content, _, err := readBoundedLog(response.Body, maxBytes)
		return content, int64(len(content)) < maxBytes, err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, false, fmt.Errorf("log download returned %d", response.StatusCode)
	}
	reachedEnd := response.StatusCode == http.StatusOK
	if response.StatusCode == http.StatusPartialContent {
		reachedEnd = false
		if contentRange := response.Header.Get("Content-Range"); contentRange != "" {
			reachedEnd = isCompleteContentRange(contentRange)
		}
	}
	content, _, err := readBoundedLog(response.Body, maxBytes)
	if err != nil {
		return nil, false, err
	}
	return content, reachedEnd && int64(len(content)) < maxBytes, nil
}

func isCompleteContentRange(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes ") {
		return false
	}
	remainder := strings.TrimPrefix(value, "bytes ")
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 {
		return false
	}
	rangePart, totalPart := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if rangePart == "*" {
		return false
	}
	bounds := strings.Split(rangePart, "-")
	if len(bounds) != 2 {
		return false
	}
	var last, total int64
	if _, err := fmt.Sscanf(bounds[1], "%d", &last); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(totalPart, "%d", &total); err != nil {
		return false
	}
	return last+1 >= total
}

func discardLogPrefix(ctx context.Context, body io.Reader, offset int64) (bool, error) {
	remaining := offset
	buffer := make([]byte, 32<<10)
	for remaining > 0 {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		read, err := io.ReadFull(body, buffer[:want])
		remaining -= int64(read)
		if err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return false, nil
			}
			return false, &TransientError{Cause: err}
		}
	}
	return true, nil
}

func readBoundedLog(body io.Reader, maxBytes int64) ([]byte, bool, error) {
	content, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, false, &TransientError{Cause: err}
	}
	if int64(len(content)) > maxBytes {
		return content[:maxBytes], false, nil
	}
	return content, true, nil
}

func (client *APIClient) GetCIJobLogExcerpt(ctx context.Context, installationToken, owner, repository string, job JobScope, stepNumber *int, cursor string, maxBytes int, repoID int64) (CILogExcerpt, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CILogExcerpt{}, err
	}
	if job.ID <= 0 {
		return CILogExcerpt{}, &ConfigurationError{Cause: fmt.Errorf("invalid job id")}
	}
	if maxBytes == 0 {
		maxBytes = DefaultCILogExcerptBytes
	}
	if maxBytes < MinCILogExcerptBytes || maxBytes > MaxCILogExcerptBytes {
		return CILogExcerpt{}, &ConfigurationError{Cause: fmt.Errorf("invalid excerpt size")}
	}
	var offset int64
	if cursor != "" {
		decoded, err := decodeCICursor(cursor)
		if err != nil {
			return CILogExcerpt{}, &ConfigurationError{Cause: err}
		}
		wantExtra := ""
		if stepNumber != nil {
			wantExtra = fmt.Sprintf("step=%d", *stepNumber)
		}
		if decoded.Kind != "job_logs" || decoded.ID != job.ID || decoded.RepoID != repoID || decoded.Head != job.HeadSHA || decoded.Extra != wantExtra {
			return CILogExcerpt{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		offset = decoded.Offset
	}
	if stepNumber != nil && (*stepNumber <= 0 || *stepNumber > 1000) {
		return CILogExcerpt{}, &ConfigurationError{Cause: fmt.Errorf("invalid step selector")}
	}
	location, metadata, err := client.jobLogsLocation(ctx, installationToken, owner, repository, job.ID)
	if err != nil {
		return CILogExcerpt{}, mapLogLocationError(ctx, metadata, err)
	}
	safeURL, err := validateLogDownloadURL(location, client.baseURL)
	if err != nil {
		return CILogExcerpt{}, responseFailure(metadata, "ci_logs_download", "invalid_url", ErrInvalidAPIResponse)
	}
	raw, _, err := fetchLogBytes(ctx, safeURL, offset, int64(maxBytes)+1)
	if err != nil {
		if ctx.Err() != nil {
			return CILogExcerpt{}, responseFailure(metadata, "ci_logs_download", CancellationReason(ctx.Err()), ctx.Err())
		}
		return CILogExcerpt{}, responseFailure(metadata, "ci_logs_download", "transport_failed", &TransientError{Cause: err})
	}
	hasMore := int64(len(raw)) > int64(maxBytes)
	var window []byte
	if hasMore {
		window = raw[:maxBytes]
	} else {
		window = raw
	}
	text := strings.ToValidUTF8(string(window), "�")
	for hasMore && !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
		window = window[:len(text)]
	}
	endOffset := offset + int64(len(window))
	excerpt := CILogExcerpt{
		HeadSHA: job.HeadSHA, RunID: job.RunID, RunAttempt: job.RunAttempt,
		JobID: job.ID, Text: text, StartOffset: offset, EndOffset: endOffset,
		HasMore: hasMore, Truncated: hasMore, Empty: len(window) == 0 && !hasMore,
	}
	if stepNumber != nil {
		excerpt.StepNumber = stepNumber
	}
	if hasMore {
		extra := ""
		if stepNumber != nil {
			extra = fmt.Sprintf("step=%d", *stepNumber)
		}
		excerpt.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "job_logs", RepoID: repoID, Head: job.HeadSHA, ID: job.ID, Offset: endOffset, Extra: extra})
	}
	if err := ensureCISerializedBound(excerpt); err != nil {
		for len(excerpt.Text) > MinCILogExcerptBytes && err != nil {
			shrink := len(excerpt.Text) / 2
			if shrink < MinCILogExcerptBytes {
				shrink = MinCILogExcerptBytes
			}
			excerpt.Text = excerpt.Text[:shrink]
			for !utf8.ValidString(excerpt.Text) && len(excerpt.Text) > 0 {
				excerpt.Text = excerpt.Text[:len(excerpt.Text)-1]
			}
			excerpt.EndOffset = excerpt.StartOffset + int64(len(excerpt.Text))
			excerpt.HasMore, excerpt.Truncated = true, true
			extra := ""
			if stepNumber != nil {
				extra = fmt.Sprintf("step=%d", *stepNumber)
			}
			excerpt.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "job_logs", RepoID: repoID, Head: job.HeadSHA, ID: job.ID, Offset: excerpt.EndOffset, Extra: extra})
			err = ensureCISerializedBound(excerpt)
		}
		if err != nil {
			return CILogExcerpt{}, err
		}
	}
	return excerpt, nil
}

func (client *APIClient) SearchCIJobLogs(ctx context.Context, installationToken, owner, repository string, job JobScope, query string, contextLines int, cursor string, repoID int64) (CILogSearchResult, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CILogSearchResult{}, err
	}
	if job.ID <= 0 {
		return CILogSearchResult{}, &ConfigurationError{Cause: fmt.Errorf("invalid job id")}
	}
	if len(query) == 0 || len(query) > MaxCIQueryLength || !utf8.ValidString(query) || strings.ContainsRune(query, '\x00') {
		return CILogSearchResult{}, &ConfigurationError{Cause: fmt.Errorf("invalid search query")}
	}
	if contextLines < 0 || contextLines > MaxCISearchContext {
		return CILogSearchResult{}, &ConfigurationError{Cause: fmt.Errorf("invalid context lines")}
	}
	var offset int64
	if cursor != "" {
		decoded, err := decodeCICursor(cursor)
		if err != nil {
			return CILogSearchResult{}, &ConfigurationError{Cause: err}
		}
		if decoded.Kind != "job_search" || decoded.ID != job.ID || decoded.RepoID != repoID || decoded.Head != job.HeadSHA || decoded.Extra != query {
			return CILogSearchResult{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		offset = decoded.Offset
	}
	location, metadata, err := client.jobLogsLocation(ctx, installationToken, owner, repository, job.ID)
	if err != nil {
		return CILogSearchResult{}, mapLogLocationError(ctx, metadata, err)
	}
	safeURL, err := validateLogDownloadURL(location, client.baseURL)
	if err != nil {
		return CILogSearchResult{}, responseFailure(metadata, "ci_logs_download", "invalid_url", ErrInvalidAPIResponse)
	}
	raw, reachedEnd, err := fetchLogBytes(ctx, safeURL, offset, MaxCILogScanBytes+1)
	if err != nil {
		if ctx.Err() != nil {
			return CILogSearchResult{}, responseFailure(metadata, "ci_logs_download", CancellationReason(ctx.Err()), ctx.Err())
		}
		return CILogSearchResult{}, responseFailure(metadata, "ci_logs_download", "transport_failed", &TransientError{Cause: err})
	}
	searchedTruncated := int64(len(raw)) > MaxCILogScanBytes
	var window []byte
	if searchedTruncated {
		window = raw[:MaxCILogScanBytes]
		reachedEnd = false
	} else {
		window = raw
	}
	text := strings.ToValidUTF8(string(window), "�")
	matches := searchLogText(text, query, contextLines, offset)
	nextOffset := offset + int64(len(window))
	if searchedTruncated {
		if lastNewline := bytes.LastIndexByte(window, '\n'); lastNewline >= 0 {
			nextOffset = offset + int64(lastNewline) + 1
			kept := matches[:0]
			for _, match := range matches {
				if match.StartOffset < nextOffset {
					kept = append(kept, match)
				}
			}
			matches = kept
		}
	}
	// searchLogText retains one sentinel match beyond the cap. Evaluate it
	// before truncating: otherwise an incomplete search reports complete
	// results and the held-back matches become unretrievable.
	overflow := len(matches) > MaxCISearchMatches
	if overflow {
		heldBack := matches[MaxCISearchMatches]
		matches = matches[:MaxCISearchMatches]
		// Resume at the held-back match's query line, not at the last
		// returned match's context end: the next query line can fall inside
		// that context window and would otherwise be skipped.
		nextOffset = queryLineByteOffset(text, offset, heldBack.LineNumber)
		if nextOffset <= offset {
			// Defensive: the 21st query line always starts past the page
			// base (at least 20 lines precede it), so this only guards
			// against a stuck cursor, never normal pagination.
			nextOffset = offset + int64(len(window))
		}
	}
	hasMore := searchedTruncated || overflow
	result := CILogSearchResult{
		HeadSHA: job.HeadSHA, JobID: job.ID, Query: query, Matches: matches,
		HasMore: hasMore || searchedTruncated, ReachedEnd: reachedEnd && !searchedTruncated,
		SearchedBytes: int64(len(window)),
	}
	if matches == nil {
		result.Matches = []CILogMatch{}
	}
	if result.HasMore {
		result.NextCursor = encodeCICursor(ciCursor{Version: 1, Kind: "job_search", RepoID: repoID, Head: job.HeadSHA, ID: job.ID, Offset: nextOffset, Extra: query})
		result.ReachedEnd = false
	}
	if err := ensureCISerializedBound(result); err != nil {
		return CILogSearchResult{}, err
	}
	return result, nil
}

func searchLogText(text, query string, contextLines int, baseOffset int64) []CILogMatch {
	lines := strings.SplitAfter(text, "\n")
	offsets := make([]int64, len(lines))
	offset := baseOffset
	for index, line := range lines {
		offsets[index] = offset
		offset += int64(len(line))
	}
	matches := make([]CILogMatch, 0)
	for index, line := range lines {
		clean := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if !strings.Contains(clean, query) {
			continue
		}
		start := index - contextLines
		if start < 0 {
			start = 0
		}
		end := index + contextLines
		if end >= len(lines) {
			end = len(lines) - 1
		}
		var builder strings.Builder
		for current := start; current <= end; current++ {
			segment := strings.TrimSuffix(strings.TrimSuffix(lines[current], "\n"), "\r")
			if len(segment) > 8<<10 {
				segment = segment[:8<<10]
			}
			builder.WriteString(segment)
			if current < end {
				builder.WriteString("\n")
			}
		}
		excerpt := builder.String()
		truncatedLine := len(clean) > 8<<10
		if len(excerpt) > (2*MaxCISearchContext+1)*(8<<10) {
			excerpt = excerpt[:(2*MaxCISearchContext+1)*(8<<10)]
			truncatedLine = true
		}
		matches = append(matches, CILogMatch{
			LineNumber: index + 1, StartLine: start + 1, EndLine: end + 1,
			StartOffset: offsets[start], EndOffset: offsets[end] + int64(len(lines[end])),
			Text: excerpt, TruncatedLine: truncatedLine,
		})
		if len(matches) >= MaxCISearchMatches+1 {
			break
		}
	}
	return matches
}

// queryLineByteOffset returns the absolute byte offset where the given
// window-relative 1-indexed line starts.
func queryLineByteOffset(text string, baseOffset int64, lineNumber int) int64 {
	if lineNumber <= 1 {
		return baseOffset
	}
	lines := strings.SplitAfter(text, "\n")
	if lineNumber-1 > len(lines) {
		return baseOffset
	}
	var advance int64
	for _, line := range lines[:lineNumber-1] {
		advance += int64(len(line))
	}
	return baseOffset + advance
}

func mapLogLocationError(ctx context.Context, metadata responseMetadata, err error) error {
	if ctx.Err() != nil {
		if reason := CancellationReason(ctx.Err()); reason != "" {
			return responseFailure(metadata, "ci_logs_location", reason, ctx.Err())
		}
	}
	if metadata.status == http.StatusGone {
		return responseFailure(metadata, "ci_logs_location", "http_rejected", &APIError{StatusCode: http.StatusGone, Message: "logs expired"})
	}
	if metadata.status == http.StatusNotFound {
		if message := apiErrorMessage(err); message != "" {
			lower := strings.ToLower(message)
			if strings.Contains(lower, "expired") || strings.Contains(lower, "removed") || strings.Contains(lower, "deleted") {
				return responseFailure(metadata, "ci_logs_location", "http_rejected", &APIError{StatusCode: http.StatusGone, Message: "logs expired"})
			}
		}
		return err
	}
	return err
}

func apiErrorMessage(err error) string {
	var api *APIError
	if extractAPIError(err, &api) && api != nil {
		return api.Message
	}
	return ""
}

func extractAPIError(err error, target **APIError) bool {
	for current := err; current != nil; current = tryUnwrap(current) {
		if api, ok := current.(*APIError); ok {
			*target = api
			return true
		}
		if permission, ok := current.(*PermissionError); ok && permission.APIError != nil {
			*target = permission.APIError
			return true
		}
		if rateLimit, ok := current.(*RateLimitError); ok && rateLimit.APIError != nil {
			*target = rateLimit.APIError
			return true
		}
	}
	return false
}

func tryUnwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if unwrapped, ok := err.(unwrapper); ok {
		return unwrapped.Unwrap()
	}
	return nil
}
