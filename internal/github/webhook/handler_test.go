package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/store"
)

func TestHandlerRejectsInvalidSignatureBeforeParsingOrWriting(t *testing.T) {
	inbox := &recordingInbox{}
	handler, err := webhook.NewHandler([]byte("webhook-secret"), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{"))
	request.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if inbox.calls != 0 {
		t.Errorf("inbox calls = %d, want 0", inbox.calls)
	}
}

func TestHandlerRejectsMissingSignatureBeforeParsingOrWriting(t *testing.T) {
	inbox := &recordingInbox{}
	handler, err := webhook.NewHandler([]byte("webhook-secret"), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(validEnvelope()))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if inbox.calls != 0 {
		t.Errorf("inbox calls = %d, want 0", inbox.calls)
	}
}

func TestHandlerDurablyAcceptsAuthenticatedDelivery(t *testing.T) {
	const secret = "webhook-secret"
	body := []byte(`{"action":"labeled","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12}}`)
	inbox := &recordingInbox{inserted: true}
	handler, err := webhook.NewHandler([]byte(secret), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	request := signedRequest(body, secret)
	request.Header.Set("X-GitHub-Delivery", "123e4567-e89b-12d3-a456-426614174000")
	request.Header.Set("X-GitHub-Event", "issues")
	request.Header.Set("X-GitHub-Hook-ID", "42")
	request.Header.Set("User-Agent", "GitHub-Hookshot/example")
	request.Header.Set("Authorization", "must-not-be-stored")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	if inbox.calls != 1 {
		t.Fatalf("inbox calls = %d, want 1", inbox.calls)
	}
	delivery := inbox.deliveries[0]
	if delivery.DeliveryID != "123e4567-e89b-12d3-a456-426614174000" || delivery.EventName != "issues" || delivery.Action != "labeled" {
		t.Errorf("delivery identity = (%q, %q, %q), want GitHub header/body identity", delivery.DeliveryID, delivery.EventName, delivery.Action)
	}
	if delivery.RepositoryID != 9123 || delivery.RepositoryOwner != "jozala" || delivery.RepositoryName != "omnigrex" {
		t.Errorf("repository = (%d, %q, %q), want (9123, %q, %q)", delivery.RepositoryID, delivery.RepositoryOwner, delivery.RepositoryName, "jozala", "omnigrex")
	}
	if delivery.IssueID != 456 || delivery.IssueNumber != 12 {
		t.Errorf("Issue identity = (%d, %d), want (456, 12)", delivery.IssueID, delivery.IssueNumber)
	}
	if string(delivery.Payload) != string(body) {
		t.Errorf("payload = %q, want exact raw body %q", delivery.Payload, body)
	}
	wantHeaders := map[string]string{
		"Content-Type":      "application/json",
		"User-Agent":        "GitHub-Hookshot/example",
		"X-GitHub-Delivery": "123e4567-e89b-12d3-a456-426614174000",
		"X-GitHub-Event":    "issues",
		"X-GitHub-Hook-ID":  "42",
	}
	if !reflect.DeepEqual(delivery.Headers, wantHeaders) {
		t.Errorf("headers = %#v, want %#v", delivery.Headers, wantHeaders)
	}
}

func TestHandlerAcceptsDuplicateDelivery(t *testing.T) {
	const secret = "webhook-secret"
	inbox := &recordingInbox{inserted: false}
	handler, err := webhook.NewHandler([]byte(secret), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := signedRequest([]byte(validEnvelope()), secret)
	request.Header.Set("X-GitHub-Delivery", validDeliveryID())
	request.Header.Set("X-GitHub-Event", "issues")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	if inbox.calls != 1 {
		t.Errorf("inbox calls = %d, want 1 deduplicating insert", inbox.calls)
	}
}

func TestHandlerDurablyAcceptsUnsupportedEventWithoutParsingPayload(t *testing.T) {
	const secret = "webhook-secret"
	inbox := &recordingInbox{inserted: true}
	handler, err := webhook.NewHandler([]byte(secret), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := signedRequest([]byte(`not JSON`), secret)
	request.Header.Set("X-GitHub-Delivery", validDeliveryID())
	request.Header.Set("X-GitHub-Event", "ping")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || inbox.calls != 1 {
		t.Fatalf("unsupported webhook = status %d, writes %d; want 202, 1", response.Code, inbox.calls)
	}
	delivery := inbox.deliveries[0]
	if delivery.EventName != "ping" || delivery.RepositoryID != 0 || delivery.Action != "" || string(delivery.Payload) != "not JSON" {
		t.Errorf("unsupported delivery = %#v", delivery)
	}
}

func TestHandlerOnlyAcceptsPost(t *testing.T) {
	inbox := &recordingInbox{}
	handler, err := webhook.NewHandler([]byte("webhook-secret"), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if allow := response.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}
	if inbox.calls != 0 {
		t.Errorf("inbox calls = %d, want 0", inbox.calls)
	}
}

func TestHandlerRejectsBodyLargerThanOneMiB(t *testing.T) {
	const secret = "webhook-secret"
	body := bytes.Repeat([]byte("x"), 1<<20+1)
	inbox := &recordingInbox{}
	handler, err := webhook.NewHandler([]byte(secret), inbox)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedRequest(body, secret))

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	if inbox.calls != 0 {
		t.Errorf("inbox calls = %d, want 0", inbox.calls)
	}
}

func TestHandlerRejectsAuthenticatedMalformedHeadersAndEnvelope(t *testing.T) {
	const (
		secret     = "webhook-secret"
		deliveryID = "123e4567-e89b-12d3-a456-426614174000"
	)
	tests := []struct {
		name       string
		body       string
		deliveryID string
		eventName  string
	}{
		{name: "missing delivery", body: validEnvelope(), eventName: "issues"},
		{name: "invalid delivery", body: validEnvelope(), deliveryID: "not-a-uuid", eventName: "issues"},
		{name: "missing event", body: validEnvelope(), deliveryID: deliveryID},
		{name: "invalid JSON", body: "{", deliveryID: deliveryID, eventName: "issues"},
		{name: "missing repository", body: `{"action":"opened"}`, deliveryID: deliveryID, eventName: "issues"},
		{name: "blank repository owner", body: `{"action":"opened","repository":{"id":9123,"name":"omnigrex","owner":{"login":"  "}}}`, deliveryID: deliveryID, eventName: "issues"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inbox := &recordingInbox{}
			handler, err := webhook.NewHandler([]byte(secret), inbox)
			if err != nil {
				t.Fatalf("NewHandler() error = %v", err)
			}
			request := signedRequest([]byte(test.body), secret)
			request.Header.Set("X-GitHub-Delivery", test.deliveryID)
			request.Header.Set("X-GitHub-Event", test.eventName)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			if inbox.calls != 0 {
				t.Errorf("inbox calls = %d, want 0", inbox.calls)
			}
		})
	}
}

type recordingInbox struct {
	calls      int
	deliveries []store.WebhookDelivery
	inserted   bool
	err        error
}

func (inbox *recordingInbox) InsertWebhookDelivery(_ context.Context, delivery store.WebhookDelivery) (bool, error) {
	inbox.calls++
	inbox.deliveries = append(inbox.deliveries, delivery)
	return inbox.inserted, inbox.err
}

func signedRequest(body []byte, secret string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(digest.Sum(nil)))
	return request
}

func validEnvelope() string {
	return `{"action":"opened","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12}}`
}
