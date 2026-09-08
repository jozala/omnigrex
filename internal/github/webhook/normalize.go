package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

// ErrMalformedPayload identifies an event-specific GitHub payload that cannot be normalized.
var ErrMalformedPayload = errors.New("malformed GitHub webhook payload")

// Delivery is the durable input consumed by Normalize.
type Delivery struct {
	DeliveryID string
	EventName  string
	Action     string
	Payload    []byte
}

// NormalizationOutcome distinguishes supported transitions from intentionally ignored deliveries.
type NormalizationOutcome string

const (
	// NormalizationSupported means Event contains a supported normalized transition.
	NormalizationSupported NormalizationOutcome = "SUPPORTED"
	// NormalizationIgnored means the authenticated delivery does not trigger a supported transition.
	NormalizationIgnored NormalizationOutcome = "IGNORED"
)

// Normalization is the result of parsing one durable delivery.
type Normalization struct {
	Outcome NormalizationOutcome
	Event   *NormalizedEvent
}

// NormalizedEvent contains only GitHub identities and revisions needed by workflow transitions.
type NormalizedEvent struct {
	DeliveryID   string        `json:"delivery_id"`
	EventName    string        `json:"event"`
	Action       string        `json:"action"`
	Repository   Repository    `json:"repository"`
	Installation *Installation `json:"installation,omitempty"`
	Sender       *Actor        `json:"sender,omitempty"`
	Issue        *Issue        `json:"issue,omitempty"`
	Label        string        `json:"label,omitempty"`
	PullRequest  *PullRequest  `json:"pull_request,omitempty"`
	Review       *Review       `json:"review,omitempty"`
}

// Repository is a durable GitHub repository identity.
type Repository struct {
	ID    int64  `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// Installation is a GitHub App installation identity.
type Installation struct {
	ID int64 `json:"id"`
}

// Actor is a GitHub user or App identity.
type Actor struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Issue is a durable GitHub Issue identity.
type Issue struct {
	ID     int64 `json:"id"`
	Number int64 `json:"number"`
}

// PullRequest is a durable GitHub Pull Request identity and revision.
type PullRequest struct {
	ID                    int64  `json:"id"`
	Number                int64  `json:"number"`
	NodeID                string `json:"node_id,omitempty"`
	BaseRef               string `json:"base_ref"`
	BaseSHA               string `json:"base_sha"`
	HeadRef               string `json:"head_ref"`
	HeadSHA               string `json:"head_sha"`
	BeforeSHA             string `json:"before_sha,omitempty"`
	WorkflowMarkerID      string `json:"workflow_marker_id,omitempty"`
	WorkflowMarkerInvalid bool   `json:"workflow_marker_invalid,omitempty"`
}

// Review is a durable GitHub Pull Request review identity and revision.
type Review struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id,omitempty"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	User     *Actor `json:"user,omitempty"`
}

// Normalize turns a durable raw delivery into a supported typed event or an ignored outcome.
func Normalize(delivery Delivery) (Normalization, error) {
	if workflowEvent(delivery.EventName) && delivery.Action == "" {
		return Normalization{}, malformed("missing action for %s event", delivery.EventName)
	}
	if !supportedEventAction(delivery.EventName, delivery.Action) {
		return Normalization{Outcome: NormalizationIgnored}, nil
	}
	var payload struct {
		Before     string `json:"before"`
		Action     string `json:"action"`
		Repository struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Installation *struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Sender *struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"sender"`
		Issue *struct {
			ID     int64 `json:"id"`
			Number int64 `json:"number"`
		} `json:"issue"`
		Label *struct {
			Name string `json:"name"`
		} `json:"label"`
		PullRequest *struct {
			ID     int64  `json:"id"`
			Number int64  `json:"number"`
			NodeID string `json:"node_id"`
			Body   string `json:"body"`
			Base   struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"base"`
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Review *struct {
			ID       int64  `json:"id"`
			NodeID   string `json:"node_id"`
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			User     *struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
			} `json:"user"`
		} `json:"review"`
	}
	if err := json.Unmarshal(delivery.Payload, &payload); err != nil {
		return Normalization{}, malformed("decode JSON: %v", err)
	}
	if payload.Action != delivery.Action {
		return Normalization{}, malformed("payload action %q does not match durable action %q", payload.Action, delivery.Action)
	}
	if (delivery.EventName == "issues" || delivery.EventName == "pull_request" || delivery.EventName == "pull_request_review") && strings.TrimSpace(delivery.Action) == "" {
		return Normalization{}, malformed("%s event has no action", delivery.EventName)
	}
	switch delivery.EventName {
	case "issues":
		switch delivery.Action {
		case "labeled":
			if payload.Label == nil || strings.TrimSpace(payload.Label.Name) == "" {
				return Normalization{}, malformed("issues.labeled has no label identity")
			}
			if payload.Label.Name != "omnigrex:run" {
				return Normalization{Outcome: NormalizationIgnored}, nil
			}
		case "closed", "reopened":
		default:
			return Normalization{Outcome: NormalizationIgnored}, nil
		}
	case "pull_request":
		if delivery.Action != "opened" && delivery.Action != "synchronize" {
			return Normalization{Outcome: NormalizationIgnored}, nil
		}
		if delivery.Action == "synchronize" && strings.TrimSpace(payload.Before) == "" {
			return Normalization{}, malformed("pull_request.synchronize has no previous head SHA")
		}
	case "pull_request_review":
		if delivery.Action != "submitted" {
			return Normalization{Outcome: NormalizationIgnored}, nil
		}
		if payload.Review == nil || strings.TrimSpace(payload.Review.State) == "" {
			return Normalization{}, malformed("pull_request_review.submitted has no review state")
		}
		if payload.Review.State != "approved" && payload.Review.State != "changes_requested" {
			return Normalization{Outcome: NormalizationIgnored}, nil
		}
	default:
		return Normalization{Outcome: NormalizationIgnored}, nil
	}
	if _, ok := canonicalUUID(delivery.DeliveryID); !ok {
		return Normalization{}, malformed("invalid delivery ID")
	}
	if payload.Repository.ID <= 0 || strings.TrimSpace(payload.Repository.Name) == "" || strings.TrimSpace(payload.Repository.Owner.Login) == "" {
		return Normalization{}, malformed("missing repository identity")
	}

	event := &NormalizedEvent{
		DeliveryID: delivery.DeliveryID,
		EventName:  delivery.EventName,
		Action:     delivery.Action,
		Repository: Repository{ID: payload.Repository.ID, Owner: payload.Repository.Owner.Login, Name: payload.Repository.Name},
	}
	if payload.Installation != nil {
		if payload.Installation.ID <= 0 {
			return Normalization{}, malformed("invalid installation identity")
		}
		event.Installation = &Installation{ID: payload.Installation.ID}
	}
	if payload.Sender != nil {
		if payload.Sender.ID <= 0 || strings.TrimSpace(payload.Sender.Login) == "" {
			return Normalization{}, malformed("invalid sender identity")
		}
		event.Sender = &Actor{ID: payload.Sender.ID, Login: payload.Sender.Login}
	}

	switch delivery.EventName {
	case "issues":
		if payload.Issue == nil || payload.Issue.ID <= 0 || payload.Issue.Number <= 0 {
			return Normalization{}, malformed("issues.%s has no Issue identity", delivery.Action)
		}
		event.Issue = &Issue{ID: payload.Issue.ID, Number: payload.Issue.Number}
		if payload.Label != nil {
			event.Label = payload.Label.Name
		}
	case "pull_request", "pull_request_review":
		if payload.PullRequest == nil || payload.PullRequest.ID <= 0 || payload.PullRequest.Number <= 0 ||
			strings.TrimSpace(payload.PullRequest.Base.Ref) == "" || strings.TrimSpace(payload.PullRequest.Base.SHA) == "" ||
			strings.TrimSpace(payload.PullRequest.Head.Ref) == "" || strings.TrimSpace(payload.PullRequest.Head.SHA) == "" {
			return Normalization{}, malformed("pull_request.%s has no complete Pull Request revision", delivery.Action)
		}
		event.PullRequest = &PullRequest{
			ID:      payload.PullRequest.ID,
			Number:  payload.PullRequest.Number,
			NodeID:  payload.PullRequest.NodeID,
			BaseRef: payload.PullRequest.Base.Ref,
			BaseSHA: payload.PullRequest.Base.SHA,
			HeadRef: payload.PullRequest.Head.Ref,
			HeadSHA: payload.PullRequest.Head.SHA,
		}
		if delivery.Action == "synchronize" {
			event.PullRequest.BeforeSHA = payload.Before
		}
		markerInspection := githubapi.InspectMarkers(payload.PullRequest.Body)
		event.PullRequest.WorkflowMarkerInvalid = markerInspection.Untrusted
		for _, marker := range markerInspection.Markers {
			workflowID, ok := canonicalUUID(marker.WorkflowID)
			if !ok {
				event.PullRequest.WorkflowMarkerInvalid = true
				continue
			}
			if event.PullRequest.WorkflowMarkerID != "" && event.PullRequest.WorkflowMarkerID != workflowID {
				event.PullRequest.WorkflowMarkerInvalid = true
				continue
			}
			event.PullRequest.WorkflowMarkerID = workflowID
		}
		if event.PullRequest.WorkflowMarkerInvalid {
			event.PullRequest.WorkflowMarkerID = ""
		}
		if delivery.EventName == "pull_request_review" {
			if payload.Review.ID <= 0 || strings.TrimSpace(payload.Review.NodeID) == "" || strings.TrimSpace(payload.Review.CommitID) == "" || payload.Review.User == nil {
				return Normalization{}, malformed("pull_request_review.submitted has no complete review identity")
			}
			event.Review = &Review{
				ID:       payload.Review.ID,
				NodeID:   payload.Review.NodeID,
				State:    payload.Review.State,
				CommitID: payload.Review.CommitID,
			}
			if payload.Review.User.ID <= 0 || strings.TrimSpace(payload.Review.User.Login) == "" {
				return Normalization{}, malformed("invalid review user identity")
			}
			event.Review.User = &Actor{ID: payload.Review.User.ID, Login: payload.Review.User.Login}
		}
	}
	return Normalization{Outcome: NormalizationSupported, Event: event}, nil
}

func supportedEventAction(eventName, action string) bool {
	switch eventName {
	case "issues":
		return action == "labeled" || action == "closed" || action == "reopened"
	case "pull_request":
		return action == "opened" || action == "synchronize"
	case "pull_request_review":
		return action == "submitted"
	default:
		return false
	}
}

func malformed(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformedPayload, fmt.Sprintf(format, arguments...))
}
