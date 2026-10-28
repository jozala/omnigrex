package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CITargetMapping is read provenance, never review or mutation authority.
// Only a fresh successful verification grants TestMergeSHA. Historical head
// reads remain possible when the current PR has moved, but no alternate target
// is granted in that case. Every new diagnostic request must reverify mapping.
type CITargetMapping struct {
	RepositoryID      int64               `json:"repository_id"`
	PullRequestID     int64               `json:"pull_request_id,omitempty"`
	PullRequestNumber int                 `json:"pull_request_number,omitempty"`
	ReviewedHead      string              `json:"reviewed_head"`
	BaseSHA           string              `json:"base_sha,omitempty"`
	BaseRef           string              `json:"base_ref,omitempty"`
	TestMergeSHA      string              `json:"test_merge_sha,omitempty"`
	State             string              `json:"state"`
	ObservedAt        time.Time           `json:"observed_at"`
	Failures          []CIEvidenceFailure `json:"failures,omitempty"`
}

// VerifyCITarget verifies both GitHub's live PR mapping and the actual merge
// commit's parents against a freshly read target branch. A queue/group commit
// or a merge checkout mentioned only in workflow text is not authority.
func (client *APIClient) VerifyCITarget(ctx context.Context, token, owner, repository string, repoID, prID int64, number int, head, headRef, baseRef string) CITargetMapping {
	mapping := CITargetMapping{RepositoryID: repoID, PullRequestID: prID, PullRequestNumber: number, ReviewedHead: head, State: "head_only", ObservedAt: time.Now().UTC()}
	fail := func(err error) CITargetMapping {
		mapping.State = "unavailable"
		mapping.Failures = []CIEvidenceFailure{evidenceFailure("test_merge_mapping", err)}
		return mapping
	}
	if !validCommitSHA(head) || repoID <= 0 {
		return fail(ErrInvalidAPIResponse)
	}
	if number == 0 {
		return mapping
	}
	pr, err := client.GetPullRequest(ctx, token, owner, repository, number)
	if err != nil {
		return fail(err)
	}
	if pr.ID != prID || pr.Head.SHA != head || pr.Head.Ref != headRef || pr.Base.Ref != baseRef || pr.Base.Repository == nil || pr.Base.Repository.ID != repoID || pr.Base.Repository.FullName != owner+"/"+repository {
		mapping.State = "stale"
		return mapping
	}
	mapping.BaseSHA = pr.Base.SHA
	mapping.BaseRef = pr.Base.Ref
	if pr.State != "open" || pr.Merged {
		return mapping
	}
	if pr.Mergeable == nil {
		mapping.Failures = []CIEvidenceFailure{{Operation: "test_merge_mapping", Code: "mergeability_pending"}}
		return mapping
	}
	if !*pr.Mergeable {
		return mapping
	}
	if pr.MergeCommitSHA == nil || !validCommitSHA(*pr.MergeCommitSHA) || *pr.MergeCommitSHA == head {
		mapping.Failures = []CIEvidenceFailure{{Operation: "test_merge_mapping", Code: "test_merge_unavailable"}}
		return mapping
	}
	root := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repository))
	var branch struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err = client.doJSON(ctx, http.MethodGet, root+"/branches/"+url.PathEscape(baseRef), token, nil, &branch)
	if err != nil {
		return fail(err)
	}
	if branch.Name != baseRef || branch.Commit.SHA != pr.Base.SHA {
		mapping.State = "stale"
		return mapping
	}
	var commit struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	err = client.doJSON(ctx, http.MethodGet, root+"/git/commits/"+*pr.MergeCommitSHA, token, nil, &commit)
	if err != nil {
		return fail(err)
	}
	if commit.SHA != *pr.MergeCommitSHA || len(commit.Parents) != 2 || commit.Parents[0].SHA != pr.Base.SHA || commit.Parents[1].SHA != head {
		mapping.State = "conflicting"
		return mapping
	}
	// Reobserve after reading the merge object: do not combine different live PR
	// mappings. Future requests repeat this verification, invalidating cursors
	// whose actual authorized commit no longer belongs to the current mapping.
	fresh, err := client.GetPullRequest(ctx, token, owner, repository, number)
	if err != nil {
		return fail(err)
	}
	if fresh.ID != pr.ID || fresh.Head.SHA != head || fresh.Base.SHA != pr.Base.SHA || fresh.Base.Ref != baseRef || fresh.Base.Repository == nil || fresh.Base.Repository.ID != repoID || fresh.MergeCommitSHA == nil || *fresh.MergeCommitSHA != *pr.MergeCommitSHA || fresh.Mergeable == nil || !*fresh.Mergeable || fresh.Merged || fresh.State != "open" {
		mapping.State = "stale"
		return mapping
	}
	mapping.TestMergeSHA = *pr.MergeCommitSHA
	mapping.State = "verified"
	return mapping
}

// GetCheckRunMetadata is used to authorize the reported commit before fetching
// diagnostic bodies. The underlying repository endpoint validates resource ID.
func (client *APIClient) GetCheckRunMetadata(ctx context.Context, token, owner, repository string, id int64) (CheckRun, error) {
	if err := validateRepository(owner, repository); err != nil {
		return CheckRun{}, err
	}
	if id <= 0 {
		return CheckRun{}, ErrInvalidAPIResponse
	}
	check, _, err := client.getCheckRun(ctx, token, owner, repository, id)
	return check, err
}

// CICursorReadHead returns only a head already authorized by the controller.
// The endpoint still validates the cursor kind, resource, attempt and offsets.
func CICursorReadHead(cursor string, mapping CITargetMapping) (string, error) {
	if cursor == "" {
		return mapping.ReviewedHead, nil
	}
	decoded, err := decodeCICursor(cursor)
	if err != nil {
		return "", &ConfigurationError{Cause: err}
	}
	if decoded.RepoID != mapping.RepositoryID || decoded.Head != mapping.ReviewedHead && (mapping.State != "verified" || decoded.Head != mapping.TestMergeSHA) {
		return "", &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	return decoded.Head, nil
}
