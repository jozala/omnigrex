package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/jozala/omnigrex/internal/collaboration"
)

type ClarificationAPI interface {
	CreateIssueComment(context.Context, string, string, string, int, CommentRequest) (IssueComment, error)
	ListIssueComments(context.Context, string, string, string, int) ([]IssueComment, error)
}

// ClarificationPublisher is the GitHub Issue adapter for outbound collaboration.
// Source IDs, reply syntax, credentials and exact marker matching stay here.
type ClarificationPublisher struct {
	API               ClarificationAPI
	Credential        func(context.Context) (string, error)
	Owner, Repository string
	IssueNumber       int
	Marker            Marker
}

func clarificationBody(round collaboration.Round) string {
	var body strings.Builder
	fmt.Fprintf(&body, "## Clarification %s\n\n", round.ID)
	if round.Supersedes != "" {
		fmt.Fprintf(&body, "Supersedes round `%s`; earlier accepted responses remain in history.\n\n", round.Supersedes)
	}
	for _, question := range round.Questions {
		fmt.Fprintf(&body, "- **%s**: %s\n", question.ID, question.Body)
	}
	fmt.Fprintf(&body, "\nReply with `/omnigrex answer %s` on the first nonblank line of a new Issue comment, followed by your complete or partial answer. Repository write permission is required. Unanswered questions remain unresolved. This is expected waiting, not a Human Handoff; do not add omnigrex:run. Answers do not authorize implementation.\n", round.ID)
	return body.String()
}

func (p ClarificationPublisher) body(round collaboration.Round, signature string) (string, string, error) {
	body := clarificationBody(round)
	inspection := InspectMarkers(body)
	if inspection.Untrusted || len(inspection.Markers) > 0 {
		return "", "", ErrUntrustedMarkerText
	}
	marker, err := RenderMarker(p.Marker)
	if err != nil {
		return "", "", err
	}
	signed := AppendSignature(body, signature)
	if err = CheckPostedBodyLength(signed, marker); err != nil {
		return "", "", err
	}
	return signed, marker, nil
}
func (p ClarificationPublisher) Validate(message collaboration.PublicationMessage) error {
	if message.WorkflowID != "" && message.WorkflowID != p.Marker.WorkflowID {
		return collaboration.ErrInvalid
	}
	_, _, err := p.body(message.Round, message.Signature)
	if err != nil {
		return fmt.Errorf("%w: %v", collaboration.ErrInvalid, err)
	}
	return nil
}
func (p ClarificationPublisher) Publish(ctx context.Context, message collaboration.PublicationMessage) (collaboration.Source, error) {
	body, marker, err := p.body(message.Round, message.Signature)
	if err != nil {
		return collaboration.Source{}, err
	}
	credential, err := p.Credential(ctx)
	if err != nil {
		return collaboration.Source{}, err
	}
	comment, err := p.API.CreateIssueComment(ctx, credential, p.Owner, p.Repository, p.IssueNumber, CommentRequest{Body: body, Marker: marker})
	if err != nil {
		return collaboration.Source{}, err
	}
	if comment.ID <= 0 || comment.NodeID == "" || comment.HTMLURL == "" {
		return collaboration.Source{}, ErrInvalidAPIResponse
	}
	return collaboration.Source{Channel: "github", ID: fmt.Sprint(comment.ID), URL: comment.HTMLURL}, nil
}
func (p ClarificationPublisher) Reconcile(ctx context.Context, message collaboration.PublicationMessage) (collaboration.Source, bool, error) {
	body, marker, err := p.body(message.Round, message.Signature)
	if err != nil {
		return collaboration.Source{}, false, err
	}
	credential, err := p.Credential(ctx)
	if err != nil {
		return collaboration.Source{}, false, err
	}
	comments, err := p.API.ListIssueComments(ctx, credential, p.Owner, p.Repository, p.IssueNumber)
	if err != nil {
		return collaboration.Source{}, false, err
	}
	var match *IssueComment
	for _, comment := range comments {
		inspection := InspectMarkers(comment.Body)
		owns := false
		for _, m := range inspection.Markers {
			if m == p.Marker {
				owns = true
			}
		}
		if !owns {
			continue
		}
		if inspection.Untrusted || comment.Body != JoinBodyParts(body, marker) || match != nil || comment.ID <= 0 || comment.NodeID == "" || comment.HTMLURL == "" {
			return collaboration.Source{}, false, nil
		}
		c := comment
		match = &c
	}
	if match == nil {
		return collaboration.Source{}, false, nil
	}
	return collaboration.Source{Channel: "github", ID: fmt.Sprint(match.ID), URL: match.HTMLURL}, true, nil
}
