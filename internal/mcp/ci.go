package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

const (
	CIDiagnosticPending               = "pending"
	CIDiagnosticMissing               = "missing"
	CIDiagnosticExpired               = "expired"
	CIDiagnosticUnavailable           = "unavailable"
	CIDiagnosticUnauthorized          = "unauthorized"
	CIDiagnosticRateLimited           = "rate_limited"
	CIDiagnosticTransient             = "transient"
	CIDiagnosticScopeRejected         = "scope_rejected"
	CIDiagnosticInvalidContinuation   = "invalid_continuation"
	CIDiagnosticDeadlineExceeded      = "deadline_exceeded"
	CIDiagnosticResourceExhausted     = "resource_exhausted"
	CIDiagnosticStepFilterUnsupported = "step_filter_unsupported"
)

var validCIDiagnosticCodes = map[string]struct{}{
	CIDiagnosticPending: {}, CIDiagnosticMissing: {}, CIDiagnosticExpired: {},
	CIDiagnosticUnavailable: {}, CIDiagnosticUnauthorized: {}, CIDiagnosticRateLimited: {},
	CIDiagnosticTransient: {}, CIDiagnosticScopeRejected: {}, CIDiagnosticInvalidContinuation: {},
	CIDiagnosticDeadlineExceeded: {}, CIDiagnosticResourceExhausted: {},
	CIDiagnosticStepFilterUnsupported: {},
}

type CIDiagnosticError struct {
	Code       string
	Reason     string
	HTTPStatus int
	RequestID  string
}

func (err *CIDiagnosticError) Error() string {
	if err == nil {
		return "ci diagnostics failed"
	}
	code := err.Code
	if _, ok := validCIDiagnosticCodes[code]; !ok {
		code = CIDiagnosticUnavailable
	}
	reason := strings.TrimSpace(err.Reason)
	if reason == "" {
		reason = code
	}
	return fmt.Sprintf("ci diagnostics %s: %s", code, reason)
}

func ciToolError(tool string, err error) string {
	switch tool {
	case ToolGetCheckRuns, ToolGetCheckRunDiagnostics, ToolListCIRuns, ToolGetCIRun, ToolGetCIJobLogs, ToolSearchCIJobLogs:
	default:
		return ""
	}
	var diagnostic *CIDiagnosticError
	if !errors.As(err, &diagnostic) || diagnostic == nil {
		return ""
	}
	message := diagnostic.Error()
	if len(message) == 0 || len(message) > 256 || strings.ContainsAny(message, "\r\n") {
		return "ci diagnostics unavailable: unavailable"
	}
	return message
}

func newCIDiagnosticError(code, reason string, httpStatus int, requestID string) *CIDiagnosticError {
	if _, ok := validCIDiagnosticCodes[code]; !ok {
		code, reason = CIDiagnosticUnavailable, "unavailable"
	}
	if reason == "" {
		reason = code
	}
	if len(reason) > 128 {
		reason = reason[:128]
	}
	if httpStatus < 400 || httpStatus > 599 {
		httpStatus = 0
	}
	if requestID != "" && !safeRequestID(requestID) {
		requestID = ""
	}
	return &CIDiagnosticError{Code: code, Reason: reason, HTTPStatus: httpStatus, RequestID: requestID}
}

func mapCIError(err error) error {
	if err == nil {
		return nil
	}
	var diagnostic *CIDiagnosticError
	if errors.As(err, &diagnostic) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return newCIDiagnosticError(CIDiagnosticDeadlineExceeded, "canceled", 0, "")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newCIDiagnosticError(CIDiagnosticDeadlineExceeded, "deadline_exceeded", 0, "")
	}
	if reason := githubapi.CancellationReason(err); reason != "" {
		fixed := reason
		if fixed == "deadline exceeded" {
			fixed = "deadline_exceeded"
		}
		return newCIDiagnosticError(CIDiagnosticDeadlineExceeded, fixed, 0, "")
	}
	detail := githubapi.SafeFailureDiagnostics(err)
	var api *githubapi.APIError
	var permission *githubapi.PermissionError
	var rateLimit *githubapi.RateLimitError
	var transient *githubapi.TransientError
	switch {
	case errors.As(err, &rateLimit):
		status, requestID := githubFailureMetadata(err)
		return newCIDiagnosticError(CIDiagnosticRateLimited, "rate_limited", status, requestID)
	case errors.As(err, &permission):
		status, requestID := githubFailureMetadata(err)
		return newCIDiagnosticError(CIDiagnosticUnauthorized, "insufficient_permissions", status, requestID)
	case errors.As(err, &transient):
		return newCIDiagnosticError(CIDiagnosticTransient, "transient", 0, detail.RequestID)
	case errors.As(err, &api):
		status := api.StatusCode
		requestID := api.RequestID
		if !safeRequestID(requestID) {
			requestID = ""
		}
		switch {
		case status == 401 || status == 403:
			return newCIDiagnosticError(CIDiagnosticUnauthorized, "unauthorized", status, requestID)
		case status == 429:
			return newCIDiagnosticError(CIDiagnosticRateLimited, "rate_limited", status, requestID)
		case status == 410:
			return newCIDiagnosticError(CIDiagnosticExpired, "expired", status, requestID)
		case status == 404:
			return newCIDiagnosticError(CIDiagnosticMissing, "missing", status, requestID)
		case status >= 500 || status == 408:
			return newCIDiagnosticError(CIDiagnosticTransient, "transient", status, requestID)
		default:
			return newCIDiagnosticError(CIDiagnosticUnavailable, "unavailable", status, requestID)
		}
	}
	var config *githubapi.ConfigurationError
	if errors.As(err, &config) {
		message := config.Error()
		if strings.Contains(message, "stale continuation") || strings.Contains(message, "continuation is invalid") {
			return newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_continuation", 0, "")
		}
		if strings.Contains(message, "invalid excerpt") || strings.Contains(message, "exceeds size limit") {
			return newCIDiagnosticError(CIDiagnosticResourceExhausted, "resource_exhausted", 0, "")
		}
		if errors.Is(err, githubapi.ErrInvalidAPIResponse) {
			if detail.Reason == "identity_mismatch" || strings.Contains(err.Error(), "head does not match") || strings.Contains(err.Error(), "identity") {
				return newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", detail.HTTPStatus, detail.RequestID)
			}
			return newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
		}
		return newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	if errors.Is(err, githubapi.ErrInvalidAPIResponse) {
		if detail.Reason == "identity_mismatch" || strings.Contains(err.Error(), "head does not match") || strings.Contains(err.Error(), "identity") {
			return newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", detail.HTTPStatus, detail.RequestID)
		}
		return newCIDiagnosticError(CIDiagnosticUnavailable, "unavailable", detail.HTTPStatus, detail.RequestID)
	}
	if errors.Is(err, ErrInvalidInvocation) {
		return newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	if errors.Is(err, ErrToolPrecondition) {
		return newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	return newCIDiagnosticError(CIDiagnosticUnavailable, "unavailable", detail.HTTPStatus, detail.RequestID)
}

func (backend *ProductionBackend) ciCredential(ctx context.Context, scope ToolScope) (string, error) {
	credential, err := backend.credential(ctx, ToolGetCheckRuns, scope.Role, scope.Repository)
	if err != nil {
		return "", newCIDiagnosticError(CIDiagnosticUnavailable, "unavailable", 0, "")
	}
	return credential, nil
}

type checkDiagnosticsArguments struct {
	CheckRunID int64  `json:"check_run_id"`
	Cursor     string `json:"cursor"`
}

type listRunsArguments struct {
	Cursor string `json:"cursor"`
}

type getRunArguments struct {
	RunID   int64  `json:"run_id"`
	Attempt *int   `json:"attempt"`
	Cursor  string `json:"cursor"`
}

type jobLogsArguments struct {
	JobID      int64  `json:"job_id"`
	StepNumber *int   `json:"step_number"`
	Cursor     string `json:"cursor"`
	MaxBytes   *int   `json:"max_bytes"`
}

type searchLogsArguments struct {
	JobID        int64  `json:"job_id"`
	Query        string `json:"query"`
	ContextLines *int   `json:"context_lines"`
	Cursor       string `json:"cursor"`
}

func decodeCIArguments(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalidInvocation
	}
	return nil
}

func (backend *ProductionBackend) getCheckRunDiagnostics(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments checkDiagnosticsArguments
	if decodeCIArguments(stripReservedSignatureArgument(invocation), &arguments) != nil || arguments.CheckRunID <= 0 || len(arguments.Cursor) > 4096 {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	credential, err := backend.ciCredential(ctx, invocation.Scope)
	if err != nil {
		return nil, err
	}
	mapping := backend.ciMapping(ctx, credential, invocation.Scope)
	actualHead := invocation.Scope.HeadSHA
	if api, ok := backend.github.(ciEvidenceAPI); ok {
		check, callErr := api.GetCheckRunMetadata(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name, arguments.CheckRunID)
		if callErr != nil {
			return nil, mapCIError(callErr)
		}
		if check.ID != arguments.CheckRunID || !allowsCIHead(mapping, check.HeadSHA) {
			return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
		}
		actualHead = check.HeadSHA
	}
	diagnostics, err := backend.github.GetCheckRunDiagnostics(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name, arguments.CheckRunID, arguments.Cursor, invocation.Scope.Repository.ID, actualHead)
	if err != nil {
		return nil, mapCIError(err)
	}
	if diagnostics.CheckID != arguments.CheckRunID || diagnostics.HeadSHA != actualHead {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	return marshalCIResult(diagnostics, mapping)
}

func (backend *ProductionBackend) listCIRuns(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments listRunsArguments
	if decodeCIArguments(stripReservedSignatureArgument(invocation), &arguments) != nil || len(arguments.Cursor) > 4096 {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	credential, err := backend.ciCredential(ctx, invocation.Scope)
	if err != nil {
		return nil, err
	}
	scope := invocation.Scope
	mapping := backend.ciMapping(ctx, credential, scope)
	head, err := githubapi.CICursorReadHead(arguments.Cursor, mapping)
	if err != nil {
		return nil, mapCIError(err)
	}
	list, err := backend.github.ListWorkflowRunsForHead(ctx, credential, scope.Repository.Owner, scope.Repository.Name, head, arguments.Cursor, scope.Repository.ID)
	if err != nil {
		return nil, mapCIError(err)
	}
	validate := func(list githubapi.CIRunList, head string) bool {
		if list.HeadSHA != head {
			return false
		}
		for _, run := range list.Runs {
			if run.HeadSHA != head || !allowsCIRun(mapping, run) {
				return false
			}
		}
		return true
	}
	if !validate(list, head) {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	type response struct {
		githubapi.CIRunList
		TestMerge        *githubapi.CIRunList `json:"test_merge,omitempty"`
		TestMergeFailure string               `json:"test_merge_failure,omitempty"`
	}
	result := response{CIRunList: list}
	if arguments.Cursor == "" && mapping.State == "verified" {
		merge, callErr := backend.github.ListWorkflowRunsForHead(ctx, credential, scope.Repository.Owner, scope.Repository.Name, mapping.TestMergeSHA, "", scope.Repository.ID)
		if callErr != nil {
			result.TestMergeFailure = CIDiagnosticUnavailable
			var diagnostic *CIDiagnosticError
			if errors.As(mapCIError(callErr), &diagnostic) {
				result.TestMergeFailure = diagnostic.Code
			}
		} else if !validate(merge, mapping.TestMergeSHA) {
			result.TestMergeFailure = "scope_rejected"
		} else {
			result.TestMerge = &merge
		}
	}
	return marshalCIResult(result, mapping)
}

func (backend *ProductionBackend) getCIRun(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments getRunArguments
	if decodeCIArguments(stripReservedSignatureArgument(invocation), &arguments) != nil || arguments.RunID <= 0 || len(arguments.Cursor) > 4096 {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	attempt := 0
	if arguments.Attempt != nil {
		if *arguments.Attempt <= 0 || *arguments.Attempt > 1000 {
			return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
		}
		attempt = *arguments.Attempt
	}
	credential, err := backend.ciCredential(ctx, invocation.Scope)
	if err != nil {
		return nil, err
	}
	mapping := backend.ciMapping(ctx, credential, invocation.Scope)
	run, err := backend.github.GetWorkflowRun(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name, arguments.RunID)
	if err != nil {
		return nil, mapCIError(err)
	}
	if run.ID != arguments.RunID || !allowsCIRun(mapping, run) {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	actualHead := run.HeadSHA
	detail, err := backend.github.GetWorkflowRunDetail(ctx, credential, invocation.Scope.Repository.Owner, invocation.Scope.Repository.Name, arguments.RunID, attempt, arguments.Cursor, invocation.Scope.Repository.ID, actualHead)
	if err != nil {
		return nil, mapCIError(err)
	}
	if detail.Run.ID != arguments.RunID || detail.Run.HeadSHA != actualHead || !allowsCIRun(mapping, detail.Run) {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	for _, job := range detail.Jobs {
		if job.HeadSHA != "" && job.HeadSHA != actualHead {
			return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
		}
		if job.RunID != arguments.RunID {
			return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
		}
	}
	return marshalCIResult(detail, mapping)
}

func (backend *ProductionBackend) getCIJobLogs(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments jobLogsArguments
	if decodeCIArguments(stripReservedSignatureArgument(invocation), &arguments) != nil || arguments.JobID <= 0 || len(arguments.Cursor) > 4096 {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	if arguments.StepNumber != nil && (*arguments.StepNumber <= 0 || *arguments.StepNumber > 1000) {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	maxBytes := githubapi.DefaultCILogExcerptBytes
	if arguments.MaxBytes != nil {
		if *arguments.MaxBytes < githubapi.MinCILogExcerptBytes || *arguments.MaxBytes > githubapi.MaxCILogExcerptBytes {
			return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
		}
		maxBytes = *arguments.MaxBytes
	}
	credential, err := backend.ciCredential(ctx, invocation.Scope)
	if err != nil {
		return nil, err
	}
	owner := invocation.Scope.Repository.Owner
	repository := invocation.Scope.Repository.Name
	job, err := backend.github.GetCIJob(ctx, credential, owner, repository, arguments.JobID)
	if err != nil {
		return nil, mapCIError(err)
	}
	mapping := backend.ciMapping(ctx, credential, invocation.Scope)
	run, runErr := backend.github.GetWorkflowRun(ctx, credential, owner, repository, job.RunID)
	if runErr != nil {
		return nil, mapCIError(runErr)
	}
	if job.ID != arguments.JobID || run.ID != job.RunID || !allowsCIRun(mapping, run) || job.HeadSHA != "" && job.HeadSHA != run.HeadSHA || job.RunAttempt <= 0 || run.RunAttempt < job.RunAttempt {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	actualHead := run.HeadSHA
	if arguments.StepNumber != nil {
		found := false
		for _, step := range job.Steps {
			if step.Number == *arguments.StepNumber {
				found = true
				break
			}
		}
		if !found {
			return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
		}
		// Step-scoped log retrieval is not supported: the provider exposes
		// only whole-job logs, and no log format observed here delimits
		// steps reliably. Report that explicitly instead of labeling
		// whole-job text with the requested step number. Identify steps
		// through get_ci_run metadata and fetch whole-job excerpts.
		return nil, newCIDiagnosticError(CIDiagnosticStepFilterUnsupported, "step_filter_unsupported", 0, "")
	}
	scope := githubapi.JobScope{ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt, HeadSHA: actualHead}
	excerpt, err := backend.github.GetCIJobLogExcerpt(ctx, credential, owner, repository, scope, arguments.Cursor, maxBytes, invocation.Scope.Repository.ID)
	if err != nil {
		mapped := mapCIError(err)
		if job.Status != "completed" {
			var diagnostic *CIDiagnosticError
			if errors.As(mapped, &diagnostic) && (diagnostic.Code == CIDiagnosticMissing || diagnostic.Code == CIDiagnosticUnavailable) {
				return nil, newCIDiagnosticError(CIDiagnosticPending, "pending", diagnostic.HTTPStatus, diagnostic.RequestID)
			}
		}
		return nil, mapped
	}
	if excerpt.HeadSHA != actualHead || excerpt.JobID != arguments.JobID {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	return marshalCIResult(excerpt, mapping)
}

func (backend *ProductionBackend) searchCIJobLogs(ctx context.Context, invocation Invocation) (json.RawMessage, error) {
	var arguments searchLogsArguments
	if decodeCIArguments(stripReservedSignatureArgument(invocation), &arguments) != nil || arguments.JobID <= 0 || len(arguments.Cursor) > 4096 {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	if len(arguments.Query) == 0 || len(arguments.Query) > githubapi.MaxCIQueryLength {
		return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
	}
	contextLines := 5
	if arguments.ContextLines != nil {
		if *arguments.ContextLines < 0 || *arguments.ContextLines > githubapi.MaxCISearchContext {
			return nil, newCIDiagnosticError(CIDiagnosticInvalidContinuation, "invalid_arguments", 0, "")
		}
		contextLines = *arguments.ContextLines
	}
	credential, err := backend.ciCredential(ctx, invocation.Scope)
	if err != nil {
		return nil, err
	}
	owner := invocation.Scope.Repository.Owner
	repository := invocation.Scope.Repository.Name
	job, err := backend.github.GetCIJob(ctx, credential, owner, repository, arguments.JobID)
	if err != nil {
		return nil, mapCIError(err)
	}
	mapping := backend.ciMapping(ctx, credential, invocation.Scope)
	run, runErr := backend.github.GetWorkflowRun(ctx, credential, owner, repository, job.RunID)
	if runErr != nil {
		return nil, mapCIError(runErr)
	}
	if job.ID != arguments.JobID || run.ID != job.RunID || !allowsCIRun(mapping, run) || job.HeadSHA != "" && job.HeadSHA != run.HeadSHA || job.RunAttempt <= 0 || run.RunAttempt < job.RunAttempt {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	actualHead := run.HeadSHA
	scope := githubapi.JobScope{ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt, HeadSHA: actualHead}
	result, err := backend.github.SearchCIJobLogs(ctx, credential, owner, repository, scope, arguments.Query, contextLines, arguments.Cursor, invocation.Scope.Repository.ID)
	if err != nil {
		mapped := mapCIError(err)
		if job.Status != "completed" {
			var diagnostic *CIDiagnosticError
			if errors.As(mapped, &diagnostic) && (diagnostic.Code == CIDiagnosticMissing || diagnostic.Code == CIDiagnosticUnavailable) {
				return nil, newCIDiagnosticError(CIDiagnosticPending, "pending", diagnostic.HTTPStatus, diagnostic.RequestID)
			}
		}
		return nil, mapped
	}
	if result.HeadSHA != actualHead || result.JobID != arguments.JobID {
		return nil, newCIDiagnosticError(CIDiagnosticScopeRejected, "scope_rejected", 0, "")
	}
	return marshalCIResult(result, mapping)
}
