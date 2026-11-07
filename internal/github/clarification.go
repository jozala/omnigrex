package github

import (
	"context"
	"errors"
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

	fmt.Fprintf(&body, "## Questions · %s\n\n", round.Reference)
	if round.Introduction != "" {
		fmt.Fprintf(&body, "%s\n\n", round.Introduction)
	}
	if round.SupersedesReference != "" {
		fmt.Fprintf(&body, "Replaces %s.\n\n", round.SupersedesReference)
	}
	for i, question := range round.Questions {
		fmt.Fprintf(&body, "%d. %s\n\n", i+1, question.Body)
	}
	fmt.Fprintf(&body, "Reply with `/omnigrex answer %s`, followed by your answer. You can agree with the recommendations, answer just part, or tell me what you would change.\n", round.Reference)

	return body.String()
}

func (p ClarificationPublisher) body(message collaboration.PublicationMessage) (string, string, error) {
	if message.Artifact != nil {
		return message.Artifact.Body, "", nil
	}
	body := clarificationBody(message.Round)
	if message.Checkpoint != nil {
		body = planningCheckpointBody(*message.Checkpoint)
	}
	inspection := InspectMarkers(body)
	if inspection.Untrusted || len(inspection.Markers) > 0 {
		return "", "", ErrUntrustedMarkerText
	}
	marker, err := RenderMarker(p.Marker)
	if err != nil {
		return "", "", err
	}
	signed := AppendSignature(body, message.Signature)
	if err = CheckPostedBodyLength(signed, marker); err != nil {
		if errors.Is(err, ErrPostedBodyTooLong) {
			return "", "", fmt.Errorf("%w: %w", collaboration.ErrPublicationTooLarge, err)
		}
		return "", "", err
	}
	return signed, marker, nil
}
func (p ClarificationPublisher) Validate(message collaboration.PublicationMessage) error {
	if message.WorkflowID != "" && message.WorkflowID != p.Marker.WorkflowID {
		return collaboration.ErrInvalid
	}
	_, _, err := p.body(message)
	if err != nil {
		return fmt.Errorf("%w: %w", collaboration.ErrInvalid, err)
	}
	return nil
}
func (p ClarificationPublisher) Publish(ctx context.Context, message collaboration.PublicationMessage) (collaboration.Source, error) {
	body, marker, err := p.body(message)
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
	body, marker, err := p.body(message)
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
	expected := JoinBodyParts(body, marker)
	if message.Artifact != nil {
		expected = message.Artifact.Body
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
		if inspection.Untrusted || comment.Body != expected || match != nil || comment.ID <= 0 || comment.NodeID == "" || comment.HTMLURL == "" {
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

func (p ClarificationPublisher) Render(message collaboration.PublicationMessage) (collaboration.RenderedPublication, error) {
	body, marker, err := p.body(message)
	if message.Artifact != nil {
		return *message.Artifact, err
	}
	return collaboration.RenderedPublication{Body: JoinPostedBody(body, marker), Marker: marker}, err
}
