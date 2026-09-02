package webhook_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/github/webhook"
)

func TestNormalizeIssuesLabeledRun(t *testing.T) {
	delivery := webhook.Delivery{
		DeliveryID: "123e4567-e89b-12d3-a456-426614174000",
		EventName:  "issues",
		Action:     "labeled",
		Payload: []byte(`{
			"action":"labeled",
			"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},
			"installation":{"id":88},
			"sender":{"id":77,"login":"octocat"},
			"issue":{"id":456,"number":12},
			"label":{"name":"omnigrex:run"}
		}`),
	}

	result, err := webhook.Normalize(delivery)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if result.Outcome != webhook.NormalizationSupported {
		t.Fatalf("outcome = %q, want %q", result.Outcome, webhook.NormalizationSupported)
	}
	if result.Event == nil {
		t.Fatal("event = nil, want normalized event")
	}
	event := result.Event
	if event.DeliveryID != delivery.DeliveryID || event.EventName != "issues" || event.Action != "labeled" {
		t.Errorf("event identity = (%q, %q, %q), want delivery identity", event.DeliveryID, event.EventName, event.Action)
	}
	if event.Repository.ID != 9123 || event.Repository.Owner != "jozala" || event.Repository.Name != "omnigrex" {
		t.Errorf("repository = %#v, want GitHub repository identity", event.Repository)
	}
	if event.Installation == nil || event.Installation.ID != 88 {
		t.Errorf("installation = %#v, want ID 88", event.Installation)
	}
	if event.Sender == nil || event.Sender.ID != 77 || event.Sender.Login != "octocat" {
		t.Errorf("sender = %#v, want octocat identity", event.Sender)
	}
	if event.Issue == nil || event.Issue.ID != 456 || event.Issue.Number != 12 {
		t.Errorf("issue = %#v, want Issue identity (456, 12)", event.Issue)
	}
	if event.Label != "omnigrex:run" {
		t.Errorf("label = %q, want %q", event.Label, "omnigrex:run")
	}
	if event.PullRequest != nil || event.Review != nil {
		t.Errorf("unrelated identities set: pull request %#v, review %#v", event.PullRequest, event.Review)
	}
}

func TestNormalizeIssueLifecycleEvents(t *testing.T) {
	for _, action := range []string{"closed", "reopened"} {
		t.Run(action, func(t *testing.T) {
			delivery := webhook.Delivery{
				DeliveryID: "123e4567-e89b-12d3-a456-426614174000",
				EventName:  "issues",
				Action:     action,
				Payload: []byte(`{
					"action":"` + action + `",
					"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},
					"issue":{"id":456,"number":12}
				}`),
			}

			result, err := webhook.Normalize(delivery)
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if result.Outcome != webhook.NormalizationSupported || result.Event == nil {
				t.Fatalf("result = %#v, want supported normalized event", result)
			}
			if result.Event.Action != action || result.Event.Issue == nil || result.Event.Issue.ID != 456 || result.Event.Issue.Number != 12 {
				t.Errorf("event = %#v, want %s event for Issue 456/12", result.Event, action)
			}
			if result.Event.Installation != nil || result.Event.Sender != nil {
				t.Errorf("optional identities = (%#v, %#v), want nil", result.Event.Installation, result.Event.Sender)
			}
		})
	}
}

func TestNormalizePullRequestRevisionEvents(t *testing.T) {
	for _, action := range []string{"opened", "synchronize"} {
		t.Run(action, func(t *testing.T) {
			delivery := webhook.Delivery{
				DeliveryID: "123e4567-e89b-12d3-a456-426614174000",
				EventName:  "pull_request",
				Action:     action,
				Payload: []byte(`{
					"action":"` + action + `",
					"before":"before123",
					"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},
					"pull_request":{
						"id":654,"number":21,"node_id":"PR_node",
						"body":"visible text\n<!-- omnigrex:v1 workflow=40000000-0000-4000-8000-000000000001 -->",
						"base":{"ref":"main","sha":"base123"},
						"head":{"ref":"feature","sha":"head456"}
					}
				}`),
			}

			result, err := webhook.Normalize(delivery)
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if result.Outcome != webhook.NormalizationSupported || result.Event == nil || result.Event.PullRequest == nil {
				t.Fatalf("result = %#v, want supported Pull Request event", result)
			}
			pullRequest := result.Event.PullRequest
			if pullRequest.ID != 654 || pullRequest.Number != 21 || pullRequest.NodeID != "PR_node" {
				t.Errorf("Pull Request identity = %#v, want 654/21/PR_node", pullRequest)
			}
			if pullRequest.BaseRef != "main" || pullRequest.BaseSHA != "base123" || pullRequest.HeadRef != "feature" || pullRequest.HeadSHA != "head456" {
				t.Errorf("Pull Request revision = %#v, want main/base123 and feature/head456", pullRequest)
			}
			if pullRequest.WorkflowMarkerID != "40000000-0000-4000-8000-000000000001" {
				t.Errorf("Workflow marker = %q, want marker Workflow UUID", pullRequest.WorkflowMarkerID)
			}
			if action == "synchronize" && pullRequest.BeforeSHA != "before123" {
				t.Errorf("synchronize before SHA = %q, want before123", pullRequest.BeforeSHA)
			}
			if action == "opened" && pullRequest.BeforeSHA != "" {
				t.Errorf("opened before SHA = %q, want omitted", pullRequest.BeforeSHA)
			}
			encoded, err := json.Marshal(result.Event)
			if err != nil {
				t.Fatalf("marshal normalized event: %v", err)
			}
			if strings.Contains(string(encoded), "visible text") || strings.Contains(string(encoded), "body") {
				t.Errorf("normalized payload retained Pull Request body: %s", encoded)
			}
			if result.Event.Issue != nil || result.Event.Review != nil {
				t.Errorf("unrelated identities set: Issue %#v, review %#v", result.Event.Issue, result.Event.Review)
			}
		})
	}
}

func TestNormalizeRejectsSynchronizeWithoutBeforeSHA(t *testing.T) {
	delivery := pullRequestDelivery("synchronize", `<!-- omnigrex:v1 workflow=40000000-0000-4000-8000-000000000001 -->`)

	if _, err := webhook.Normalize(delivery); !errors.Is(err, webhook.ErrMalformedPayload) {
		t.Fatalf("Normalize() error = %v, want ErrMalformedPayload", err)
	}
}

func TestNormalizeRejectsConflictingWorkflowMarkers(t *testing.T) {
	body := `<!-- omnigrex:v1 workflow=40000000-0000-4000-8000-000000000001 -->
<!-- omnigrex:v1 workflow=40000000-0000-4000-8000-000000000002 -->`
	delivery := pullRequestDelivery("opened", body)

	if _, err := webhook.Normalize(delivery); !errors.Is(err, webhook.ErrMalformedPayload) {
		t.Fatalf("Normalize() error = %v, want ErrMalformedPayload", err)
	}
}

func TestNormalizeRejectsNonUUIDWorkflowMarker(t *testing.T) {
	delivery := pullRequestDelivery("opened", `<!-- omnigrex:v1 workflow=not-a-uuid -->`)

	if _, err := webhook.Normalize(delivery); !errors.Is(err, webhook.ErrMalformedPayload) {
		t.Fatalf("Normalize() error = %v, want ErrMalformedPayload", err)
	}
}

func TestNormalizeSubmittedReviewDecisions(t *testing.T) {
	for _, state := range []string{"approved", "changes_requested"} {
		t.Run(state, func(t *testing.T) {
			delivery := webhook.Delivery{
				DeliveryID: "123e4567-e89b-12d3-a456-426614174000",
				EventName:  "pull_request_review",
				Action:     "submitted",
				Payload: []byte(`{
					"action":"submitted",
					"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},
					"pull_request":{
						"id":654,"number":21,"node_id":"PR_node",
						"base":{"ref":"main","sha":"base123"},
						"head":{"ref":"feature","sha":"head456"}
					},
					"review":{
						"id":987,"node_id":"PRR_node","state":"` + state + `","commit_id":"head456",
						"user":{"id":77,"login":"reviewer-app[bot]"}
					}
				}`),
			}

			result, err := webhook.Normalize(delivery)
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if result.Outcome != webhook.NormalizationSupported || result.Event == nil || result.Event.PullRequest == nil || result.Event.Review == nil {
				t.Fatalf("result = %#v, want supported Pull Request review event", result)
			}
			review := result.Event.Review
			if review.ID != 987 || review.NodeID != "PRR_node" || review.State != state || review.CommitID != "head456" {
				t.Errorf("review = %#v, want persisted %s review identity", review, state)
			}
			if review.User == nil || review.User.ID != 77 || review.User.Login != "reviewer-app[bot]" {
				t.Errorf("review user = %#v, want Reviewer App identity", review.User)
			}
			if result.Event.PullRequest.HeadSHA != "head456" {
				t.Errorf("Pull Request head SHA = %q, want head456", result.Event.PullRequest.HeadSHA)
			}
		})
	}
}

func TestNormalizeReturnsExplicitIgnoredOutcome(t *testing.T) {
	const repository = `"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}}`
	tests := []webhook.Delivery{
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "labeled", Payload: []byte(`{"action":"labeled",` + repository + `,"label":{"name":"bug"}}`)},
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "opened", Payload: []byte(`{"action":"opened",` + repository + `}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request", Action: "closed", Payload: []byte(`{"action":"closed",` + repository + `}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request_review", Action: "dismissed", Payload: []byte(`{"action":"dismissed",` + repository + `}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request_review", Action: "submitted", Payload: []byte(`{"action":"submitted",` + repository + `,"review":{"state":"commented"}}`)},
		{DeliveryID: validDeliveryID(), EventName: "push", Payload: []byte(`{` + repository + `}`)},
	}

	for _, delivery := range tests {
		result, err := webhook.Normalize(delivery)
		if err != nil {
			t.Errorf("Normalize(%s.%s) error = %v", delivery.EventName, delivery.Action, err)
			continue
		}
		if result.Outcome != webhook.NormalizationIgnored || result.Event != nil {
			t.Errorf("Normalize(%s.%s) = %#v, want explicit ignored outcome", delivery.EventName, delivery.Action, result)
		}
	}
}

func TestNormalizeDoesNotParseUnsupportedEventPayload(t *testing.T) {
	result, err := webhook.Normalize(webhook.Delivery{
		DeliveryID: validDeliveryID(), EventName: "ping", Payload: []byte(`not JSON`),
	})
	if err != nil {
		t.Fatalf("Normalize() unsupported event error = %v", err)
	}
	if result.Outcome != webhook.NormalizationIgnored || result.Event != nil {
		t.Errorf("Normalize() unsupported event = %#v, want ignored", result)
	}
}

func TestNormalizeReturnsExplicitFailureForMalformedSupportedEvent(t *testing.T) {
	const repository = `"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}}`
	tests := []webhook.Delivery{
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "closed", Payload: []byte(`{`)},
		{DeliveryID: validDeliveryID(), EventName: "issues", Payload: []byte(`{` + repository + `}`)},
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "closed", Payload: []byte(`{"action":"reopened",` + repository + `}`)},
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "closed", Payload: []byte(`{"action":"closed",` + repository + `}`)},
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "labeled", Payload: []byte(`{"action":"labeled",` + repository + `,"issue":{"id":456,"number":12}}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request", Action: "opened", Payload: []byte(`{"action":"opened",` + repository + `,"pull_request":{"id":654,"number":21,"base":{"ref":"main","sha":"base123"},"head":{"ref":"feature"}}}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request_review", Action: "submitted", Payload: []byte(`{"action":"submitted",` + repository + `,"pull_request":{"id":654,"number":21,"base":{"ref":"main","sha":"base123"},"head":{"ref":"feature","sha":"head456"}},"review":{"state":"approved","commit_id":"head456"}}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request_review", Action: "submitted", Payload: []byte(`{"action":"submitted",` + repository + `,"pull_request":{"id":654,"number":21,"base":{"ref":"main","sha":"base123"},"head":{"ref":"feature","sha":"head456"}},"review":{"id":987,"node_id":"PRR_node","state":"approved","commit_id":"head456"}}`)},
		{DeliveryID: validDeliveryID(), EventName: "pull_request_review", Action: "submitted", Payload: []byte(`{"action":"submitted",` + repository + `,"pull_request":{"id":654,"number":21,"base":{"ref":"main","sha":"base123"},"head":{"ref":"feature","sha":"head456"}},"review":{"id":987,"state":"approved","commit_id":"head456","user":{"id":77,"login":"reviewer"}}}`)},
		{DeliveryID: validDeliveryID(), EventName: "issues", Action: "closed", Payload: []byte(`{"action":"closed",` + repository + `,"issue":{"id":456,"number":12},"sender":{"id":0,"login":""}}`)},
	}

	for _, delivery := range tests {
		result, err := webhook.Normalize(delivery)
		if !errors.Is(err, webhook.ErrMalformedPayload) {
			t.Errorf("Normalize(%s.%s) error = %v, want ErrMalformedPayload", delivery.EventName, delivery.Action, err)
		}
		if result.Event != nil {
			t.Errorf("Normalize(%s.%s) event = %#v, want nil after malformed input", delivery.EventName, delivery.Action, result.Event)
		}
	}
}

func pullRequestDelivery(action, body string) webhook.Delivery {
	before := ""
	if action == "synchronize" {
		before = `,"before":""`
	}
	return webhook.Delivery{
		DeliveryID: validDeliveryID(),
		EventName:  "pull_request",
		Action:     action,
		Payload: []byte(`{"action":"` + action + `"` + before + `,
			"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},
			"pull_request":{"id":654,"number":21,"body":` + strconv.Quote(body) + `,
				"base":{"ref":"main","sha":"base123"},"head":{"ref":"feature","sha":"head456"}}}`),
	}
}

func validDeliveryID() string {
	return "123e4567-e89b-12d3-a456-426614174000"
}
