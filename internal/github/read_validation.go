package github

import "strings"

func validateIssueResponse(issue Issue, number int, htmlPath string) error {
	switch {
	case issue.ID <= 0:
		return validationError("invalid_id", "issue.id")
	case issue.NodeID == "":
		return validationError("missing_field", "issue.node_id")
	case issue.Number != number:
		return validationError("identity_mismatch", "issue.number")
	case strings.TrimSpace(issue.Title) == "":
		return validationError("missing_field", "issue.title")
	case !validIssueState(issue.State):
		return validationError("invalid_state", "issue.state")
	case !validResponseURL(issue.HTMLURL):
		return validationError("invalid_url", "issue.html_url")
	case !resourcePathMatches(issue.HTMLURL, htmlPath):
		return validationError("identity_mismatch", "issue.html_url")
	default:
		return nil
	}
}

func validateGraphQLRepositoryIdentity(actual *graphQLRepositoryIdentity, owner, repository, id string) error {
	switch {
	case actual == nil:
		return validationError("missing_field", "pull_request.repository")
	case actual.ID != id:
		return validationError("identity_mismatch", "pull_request.repository.id")
	case actual.Name != repository:
		return validationError("identity_mismatch", "pull_request.repository.name")
	case actual.Owner == nil:
		return validationError("missing_field", "pull_request.repository.owner")
	case actual.Owner.Login != owner:
		return validationError("identity_mismatch", "pull_request.repository.owner.login")
	default:
		return nil
	}
}

func validateReviewThreadContinuation(first, second *graphQLReviewThread, owner, repository string, number int, repositoryID, pullRequestID string) error {
	if err := validateGraphQLReviewThread(second); err != nil {
		return err
	}
	for _, check := range []struct {
		matches bool
		field   string
	}{
		{first.ID == second.ID, "review_thread.id"},
		{first.Path == second.Path, "review_thread.path"},
		{*first.IsResolved == *second.IsResolved, "review_thread.isResolved"},
		{*first.IsOutdated == *second.IsOutdated, "review_thread.isOutdated"},
		{first.DiffSide == second.DiffSide, "review_thread.diffSide"},
		{equalOptionalString(first.StartDiffSide, second.StartDiffSide), "review_thread.startDiffSide"},
		{equalOptionalInt(first.Line, second.Line), "review_thread.line"},
		{equalOptionalInt(first.StartLine, second.StartLine), "review_thread.startLine"},
		{equalOptionalInt(first.OriginalLine, second.OriginalLine), "review_thread.originalLine"},
		{equalOptionalInt(first.OriginalStartLine, second.OriginalStartLine), "review_thread.originalStartLine"},
		{first.SubjectType == second.SubjectType, "review_thread.subjectType"},
	} {
		if !check.matches {
			return validationError("identity_changed", check.field)
		}
	}
	if *second.Comments.TotalCount != *first.Comments.TotalCount {
		return validationError("count_changed", "review_comments.totalCount")
	}
	actual := second.PullRequest
	switch {
	case actual == nil:
		return validationError("missing_field", "pull_request")
	case actual.ID != pullRequestID:
		return validationError("identity_mismatch", "pull_request.id")
	case actual.Number == nil:
		return validationError("missing_field", "pull_request.number")
	case *actual.Number != number:
		return validationError("identity_mismatch", "pull_request.number")
	}
	return validateGraphQLRepositoryIdentity(actual.Repository, owner, repository, repositoryID)
}

func validateGraphQLReviewComment(source *graphQLReviewComment, thread *graphQLReviewThread, htmlPath string) error {
	switch {
	case source == nil:
		return validationError("missing_field", "review_comment")
	case !validGraphQLNodeID(source.ID):
		return validationError("invalid_node_id", "review_comment.id")
	case source.FullDatabaseID == nil:
		return validationError("missing_field", "review_comment.fullDatabaseId")
	case *source.FullDatabaseID <= 0:
		return validationError("invalid_id", "review_comment.fullDatabaseId")
	case strings.TrimSpace(source.Body) == "":
		return validationError("missing_field", "review_comment.body")
	case source.Path != thread.Path:
		return validationError("location_mismatch", "review_comment.path")
	case source.Commit == nil:
		return validationError("missing_field", "review_comment.commit")
	case !validCommitSHA(source.Commit.OID):
		return validationError("invalid_commit", "review_comment.commit.oid")
	case source.OriginalCommit == nil:
		return validationError("missing_field", "review_comment.originalCommit")
	case !validCommitSHA(source.OriginalCommit.OID):
		return validationError("invalid_commit", "review_comment.originalCommit.oid")
	case !graphQLResourcePathMatches(source.URL, htmlPath):
		return validationError("invalid_url", "review_comment.url")
	case source.Author == nil:
		return validationError("missing_field", "review_comment.author")
	case !validGraphQLNodeID(source.Author.Login):
		return validationError("invalid_identity", "review_comment.author.login")
	case source.CreatedAt == nil:
		return validationError("missing_field", "review_comment.createdAt")
	case source.CreatedAt.IsZero():
		return validationError("invalid_timestamp", "review_comment.createdAt")
	case source.UpdatedAt == nil:
		return validationError("missing_field", "review_comment.updatedAt")
	case source.UpdatedAt.IsZero():
		return validationError("invalid_timestamp", "review_comment.updatedAt")
	case source.UpdatedAt.Before(*source.CreatedAt):
		return validationError("timestamp_order", "review_comment.updatedAt")
	case source.PullRequestReview != nil && source.PullRequestReview.FullDatabaseID == nil:
		return validationError("missing_field", "review_comment.pullRequestReview.fullDatabaseId")
	case source.PullRequestReview != nil && *source.PullRequestReview.FullDatabaseID <= 0:
		return validationError("invalid_id", "review_comment.pullRequestReview.fullDatabaseId")
	default:
		return nil
	}
}
