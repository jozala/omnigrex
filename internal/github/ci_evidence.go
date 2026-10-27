package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Availability is independent of completion and success. An unavailable inventory
// must never be interpreted as a successfully discovered empty policy.
type CIAvailability string

const (
	CIAvailable   CIAvailability = "available"
	CIPartial     CIAvailability = "partial"
	CIUnavailable CIAvailability = "unavailable"
)

type CIResultState string

const (
	CIMissing            CIResultState = "missing"
	CIPending            CIResultState = "pending"
	CITerminalSuccess    CIResultState = "terminal_success"
	CITerminalNonSuccess CIResultState = "terminal_non_success"
	CIUnresolved         CIResultState = "unresolved"
	CIResultUnavailable  CIResultState = "unavailable"
)

// CISelector is shared with future explicit-selection policy. Source is either
// check, status or empty (GitHub context semantics, including collisions).
type CISelector struct {
	Context string `json:"context"`
	Source  string `json:"source,omitempty"`
	AppID   int64  `json:"app_id,omitempty"`
}
type CIRequirement struct {
	Context    string `json:"context"`
	AppID      int64  `json:"app_id,omitempty"`
	Source     string `json:"source"`
	RulesetID  int64  `json:"ruleset_id,omitempty"`
	SourceType string `json:"source_type,omitempty"`
}
type CIUnresolvedRule struct {
	Type      string `json:"type"`
	Source    string `json:"source"`
	RulesetID int64  `json:"ruleset_id,omitempty"`
	Reason    string `json:"reason"`
}
type CIEvidenceFailure struct {
	Operation string `json:"operation"`
	Code      string `json:"code"`
}
type CIInventory struct {
	Repository   string              `json:"repository"`
	BranchSHA    string              `json:"branch_sha,omitempty"`
	Availability CIAvailability      `json:"availability"`
	Branch       string              `json:"branch"`
	ObservedAt   time.Time           `json:"observed_at"`
	Requirements []CIRequirement     `json:"requirements"`
	Unresolved   []CIUnresolvedRule  `json:"unresolved"`
	Failures     []CIEvidenceFailure `json:"failures"`
}
type CheckApp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug,omitempty"`
	Name string `json:"name,omitempty"`
}

// Commit statuses do not expose an integration App identity. Creator identity
// must not be substituted for the producer restriction of a required context.
type CommitStatus struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Context   string    `json:"context"`
	State     string    `json:"state"`
	URL       string    `json:"url"`
	TargetURL string    `json:"target_url,omitempty"`
	Creator   *User     `json:"creator,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type CIObservation struct {
	Availability CIAvailability      `json:"availability"`
	RepositoryID int64               `json:"repository_id"`
	HeadSHA      string              `json:"head_sha"`
	ObservedAt   time.Time           `json:"observed_at"`
	Checks       []CheckRun          `json:"checks"`
	Statuses     []CommitStatus      `json:"statuses"`
	Failures     []CIEvidenceFailure `json:"failures"`
}
type CIRequirementResult struct {
	Requirement CIRequirement `json:"requirement"`
	Source      string        `json:"source"`
	State       CIResultState `json:"state"`
	ResultID    int64         `json:"result_id,omitempty"`
	Conclusion  string        `json:"conclusion,omitempty"`
	Reason      string        `json:"reason,omitempty"`
}

func evidenceFailure(operation string, err error) CIEvidenceFailure {
	code := "unavailable"
	var permission *PermissionError
	var rate *RateLimitError
	var transient *TransientError
	var api *APIError
	switch {
	case errors.As(err, &rate):
		code = "rate_limited"
	case errors.As(err, &permission):
		code = "insufficient_permissions"
	case errors.As(err, &transient):
		code = "transient"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code = "deadline_exceeded"
	case errors.As(err, &api):
		switch {
		case api.StatusCode == 401 || api.StatusCode == 403:
			code = "insufficient_permissions"
		case api.StatusCode == 429:
			code = "rate_limited"
		case api.StatusCode >= 500:
			code = "transient"
		case api.StatusCode == 404:
			code = "inaccessible"
		}
	case errors.Is(err, ErrInvalidAPIResponse):
		code = "inconsistent_response"
	}
	return CIEvidenceFailure{Operation: operation, Code: code}
}

// ciArray reads the full collection with a finite safety bound. Continuations
// cannot change the endpoint, commit, or query selectors.
func ciArray[T any](client *APIClient, ctx context.Context, token, path string, validate func(T) error) ([]T, error) {
	firstPath := path
	result := make([]T, 0)
	seen := map[string]bool{}
	for path != "" {
		if seen[path] || len(seen) >= 100 {
			return nil, fmt.Errorf("%w: evidence pagination exhausted", ErrInvalidAPIResponse)
		}
		seen[path] = true
		var page []T
		header, err := client.doJSONWithHeaders(ctx, http.MethodGet, path, token, nil, &page)
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("%w: missing evidence array", ErrInvalidAPIResponse)
		}
		for _, item := range page {
			if err := validate(item); err != nil {
				return nil, err
			}
			result = append(result, item)
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		path, err = client.ciPaginationPath(firstPath, next)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// DiscoverRequiredCI describes current policy, not policy at a historical PR
// baseline. Effective branch rules include inherited active rulesets only;
// disabled/evaluate rules are excluded by that documented GitHub endpoint.
func (client *APIClient) DiscoverRequiredCI(ctx context.Context, token, owner, repository, branch string) CIInventory {
	inventory := CIInventory{Availability: CIAvailable, Repository: owner + "/" + repository, Branch: branch, ObservedAt: time.Now().UTC(), Requirements: []CIRequirement{}, Unresolved: []CIUnresolvedRule{}, Failures: []CIEvidenceFailure{}}
	fail := func(operation string, err error) {
		inventory.Availability = CIPartial
		inventory.Failures = append(inventory.Failures, evidenceFailure(operation, err))
	}
	if err := validateRepository(owner, repository); err != nil {
		fail("policy", err)
		return inventory
	}
	if !validGitRefName(branch) {
		fail("policy", ErrInvalidAPIResponse)
		return inventory
	}
	root := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repository))
	var observed struct {
		Name      string `json:"name"`
		Protected *bool  `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := client.doJSON(ctx, http.MethodGet, root+"/branches/"+url.PathEscape(branch), token, nil, &observed)
	if err != nil {
		fail("classic", err)
	} else if observed.Name != branch || observed.Protected == nil || !validCommitSHA(observed.Commit.SHA) {
		fail("classic", ErrInvalidAPIResponse)
	} else if *observed.Protected {
		inventory.BranchSHA = observed.Commit.SHA
		var protection struct {
			RequiredStatusChecks json.RawMessage `json:"required_status_checks"`
		}
		var classic struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
				AppID   *int64 `json:"app_id"`
			} `json:"checks"`
		}
		err = client.doJSON(ctx, http.MethodGet, root+"/branches/"+url.PathEscape(branch)+"/protection", token, nil, &protection)
		// Only GitHub's explicit no-classic-protection response establishes
		// absence on a readable branch (including ruleset-only protection).
		// Generic 404s remain inaccessible, never an asserted empty policy.
		var apiError *APIError
		noClassic := errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound && strings.EqualFold(strings.TrimSpace(apiError.Message), "Branch not protected")
		if noClassic {
		} else if err != nil {
			fail("classic", err)
		} else if len(protection.RequiredStatusChecks) == 0 {
			fail("classic", ErrInvalidAPIResponse)
		} else if string(protection.RequiredStatusChecks) == "null" {
			// Explicit absence in a successful full protection response is empty policy.
		} else if json.Unmarshal(protection.RequiredStatusChecks, &classic) != nil || classic.Contexts == nil || classic.Checks == nil {
			fail("classic", ErrInvalidAPIResponse)
		} else {
			names := map[string]bool{}
			for _, check := range classic.Checks {
				if strings.TrimSpace(check.Context) == "" || check.AppID != nil && *check.AppID < -1 {
					fail("classic", ErrInvalidAPIResponse)
					continue
				}
				app := int64(0)
				if check.AppID != nil && *check.AppID > 0 {
					app = *check.AppID
				}
				inventory.Requirements = append(inventory.Requirements, CIRequirement{Context: check.Context, AppID: app, Source: "classic"})
				names[check.Context] = true
			}
			for _, name := range classic.Contexts {
				if strings.TrimSpace(name) == "" {
					fail("classic", ErrInvalidAPIResponse)
					continue
				}
				if !names[name] {
					inventory.Requirements = append(inventory.Requirements, CIRequirement{Context: name, Source: "classic"})
				}
			}
		}
	}
	if inventory.BranchSHA == "" && validCommitSHA(observed.Commit.SHA) {
		inventory.BranchSHA = observed.Commit.SHA
	}
	type rule struct {
		Type       string `json:"type"`
		RulesetID  int64  `json:"ruleset_id"`
		SourceType string `json:"ruleset_source_type"`
		Source     string `json:"ruleset_source"`
		Parameters *struct {
			RequiredStatusChecks []struct {
				Context       string `json:"context"`
				IntegrationID *int64 `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	rules, err := ciArray(client, ctx, token, root+"/rules/branches/"+url.PathEscape(branch)+"?per_page=100", func(item rule) error {
		if item.Type == "" || item.RulesetID <= 0 || item.Source == "" || item.SourceType == "" {
			return ErrInvalidAPIResponse
		}
		return nil
	})
	if err != nil {
		fail("effective_rules", err)
	} else {
		for _, rule := range rules {
			switch rule.Type {
			case "required_status_checks":
				if rule.Parameters == nil || rule.Parameters.RequiredStatusChecks == nil {
					fail("effective_rules", ErrInvalidAPIResponse)
					continue
				}
				for _, check := range rule.Parameters.RequiredStatusChecks {
					if strings.TrimSpace(check.Context) == "" || check.IntegrationID != nil && *check.IntegrationID < 0 {
						fail("effective_rules", ErrInvalidAPIResponse)
						continue
					}
					app := int64(0)
					if check.IntegrationID != nil {
						app = *check.IntegrationID
					}
					inventory.Requirements = append(inventory.Requirements, CIRequirement{Context: check.Context, AppID: app, Source: rule.Source, SourceType: rule.SourceType, RulesetID: rule.RulesetID})
				}
			case "workflows", "required_workflows":
				inventory.Unresolved = append(inventory.Unresolved, CIUnresolvedRule{Type: rule.Type, Source: rule.Source, RulesetID: rule.RulesetID, Reason: "workflow_completion_mapping_unsupported"})
			case "creation", "update", "deletion", "required_linear_history", "required_signatures", "pull_request", "required_deployments", "non_fast_forward", "merge_queue", "file_path_restriction", "max_file_path_length", "file_extension_restriction", "max_file_size":
				// Non-CI merge policy is deliberately outside this inventory.
			default:
				inventory.Unresolved = append(inventory.Unresolved, CIUnresolvedRule{Type: rule.Type, Source: rule.Source, RulesetID: rule.RulesetID, Reason: "rule_completion_mapping_unsupported"})
			}
		}
	}
	if len(inventory.Failures) > 0 && len(inventory.Requirements) == 0 && len(inventory.Unresolved) == 0 {
		inventory.Availability = CIUnavailable
	}
	return inventory
}

func (client *APIClient) GetCommitStatuses(ctx context.Context, token, owner, repository, head string) ([]CommitStatus, error) {
	if err := validateRepository(owner, repository); err != nil {
		return nil, err
	}
	if !validCommitSHA(head) {
		return nil, &ConfigurationError{Cause: ErrInvalidCommitSHA}
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/statuses?per_page=100", url.PathEscape(owner), url.PathEscape(repository), head)
	seen := map[int64]bool{}
	statuses, err := ciArray(client, ctx, token, path, func(status CommitStatus) error {
		if status.ID <= 0 || status.NodeID == "" || strings.TrimSpace(status.Context) == "" || status.CreatedAt.IsZero() || status.UpdatedAt.IsZero() || !validResponseURL(status.URL) || !resourcePathMatches(status.URL, fmt.Sprintf("/repos/%s/%s/statuses/%s", owner, repository, head)) || status.TargetURL != "" && !validResponseURL(status.TargetURL) || seen[status.ID] {
			return fmt.Errorf("%w: invalid status identity", ErrInvalidAPIResponse)
		}
		switch status.State {
		case "pending", "success", "failure", "error":
		default:
			return ErrInvalidAPIResponse
		}
		seen[status.ID] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range statuses {
		// Never add signed URLs or provider credentials to the control record.
		// The canonical status identity URL is retained; external target URLs with
		// query/fragment material are omitted rather than followed or rewritten.
		parsed, _ := url.Parse(statuses[i].URL)
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, ErrInvalidAPIResponse
		}
		if statuses[i].TargetURL != "" {
			target, _ := url.Parse(statuses[i].TargetURL)
			if target.RawQuery != "" || target.Fragment != "" {
				statuses[i].TargetURL = ""
			}
		}
	}
	return statuses, nil

}

// Observation deliberately accepts no inventory and never rediscovers policy.
func (client *APIClient) ObserveCI(ctx context.Context, token, owner, repository, head string, repoID int64) CIObservation {
	observed := CIObservation{Availability: CIAvailable, RepositoryID: repoID, HeadSHA: head, ObservedAt: time.Now().UTC(), Checks: []CheckRun{}, Statuses: []CommitStatus{}, Failures: []CIEvidenceFailure{}}
	var err error
	observed.Checks, err = client.GetCheckRuns(ctx, token, owner, repository, head)
	if err != nil {
		observed.Failures = append(observed.Failures, evidenceFailure("checks", err))
		observed.Availability = CIPartial
	}
	observed.Statuses, err = client.GetCommitStatuses(ctx, token, owner, repository, head)
	if err != nil {
		observed.Failures = append(observed.Failures, evidenceFailure("statuses", err))
		observed.Availability = CIPartial
	}
	if len(observed.Failures) == 2 {
		observed.Availability = CIUnavailable
	}
	return observed
}

// MatchRequiredCI is pure: a frozen inventory can be matched against fresh
// results. Highest resource ID selects the current result per producer; GitHub
// check/status creation IDs are monotonic and queued reruns may lack timestamps.
// A context present as both a check and a status yields two independent results.
func MatchRequiredCI(inventory CIInventory, observed CIObservation) []CIRequirementResult {
	results := make([]CIRequirementResult, 0)
	failed := func(source string) bool {
		for _, failure := range observed.Failures {
			if failure.Operation == source {
				return true
			}
		}
		return false
	}
	for _, required := range inventory.Requirements {
		checks := map[int64]CheckRun{}
		var status *CommitStatus
		unknown := false
		for _, check := range observed.Checks {
			if check.Name != required.Context {
				continue
			}
			if check.App == nil || check.App.ID <= 0 {
				unknown = true
				continue
			}
			if required.AppID != 0 && check.App.ID != required.AppID {
				continue
			}
			if prior, ok := checks[check.App.ID]; !ok || check.ID > prior.ID {
				checks[check.App.ID] = check
			}
		}
		for i := range observed.Statuses {
			item := &observed.Statuses[i]
			if item.Context == required.Context && (status == nil || item.ID > status.ID) {
				status = item
			}
		}
		producers := make([]int64, 0, len(checks))
		for app := range checks {
			producers = append(producers, app)
		}
		sort.Slice(producers, func(i, j int) bool { return producers[i] < producers[j] })
		for _, app := range producers {
			check := checks[app]
			state := CIPending
			if check.Status == "completed" {
				state = CITerminalNonSuccess
				if check.Conclusion == "success" {
					state = CITerminalSuccess
				}
			}
			results = append(results, CIRequirementResult{Requirement: required, Source: "check", State: state, ResultID: check.ID, Conclusion: check.Conclusion})
		}
		if unknown {
			results = append(results, CIRequirementResult{Requirement: required, Source: "check", State: CIUnresolved, Reason: "producer_identity_unavailable"})
		}
		if failed("checks") {
			results = append(results, CIRequirementResult{Requirement: required, Source: "check", State: CIResultUnavailable, Reason: "observation_failed"})
		}
		if status != nil {
			result := CIRequirementResult{Requirement: required, Source: "status", ResultID: status.ID, Conclusion: status.State}
			switch {
			case required.AppID != 0:
				result.State = CIUnresolved
				result.Reason = "status_producer_unprovable"
			case status.State == "pending":
				result.State = CIPending
			case status.State == "success":
				result.State = CITerminalSuccess
			default:
				result.State = CITerminalNonSuccess
			}
			results = append(results, result)
		}
		if failed("statuses") {
			results = append(results, CIRequirementResult{Requirement: required, Source: "status", State: CIResultUnavailable, Reason: "observation_failed"})
		}
		if len(checks) == 0 && !unknown && status == nil && !failed("checks") && !failed("statuses") {
			results = append(results, CIRequirementResult{Requirement: required, Source: "context", State: CIMissing})
		}
	}
	return results
}

func (client *APIClient) ciPaginationPath(firstPath, next string) (string, error) {
	path, err := client.paginationPath(next)
	if err != nil {
		return "", err
	}
	first, _ := url.Parse(firstPath)
	parsed, err := url.Parse(path)
	if err != nil || parsed.EscapedPath() != first.EscapedPath() {
		return "", fmt.Errorf("%w: evidence pagination identity mismatch", ErrInvalidAPIResponse)
	}
	q := parsed.Query()
	q.Del("page")
	initial := first.Query()
	initial.Del("page")
	if q.Encode() != initial.Encode() {
		return "", fmt.Errorf("%w: evidence pagination query mismatch", ErrInvalidAPIResponse)
	}
	return path, nil
}
