package webhook_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jozala/omnigrex/internal/github/webhook"
)

func TestHandlerAcceptsProvisioningDeliveryWithoutRepositoryIdentity(t *testing.T) {
	const secret = "webhook-secret"
	body := []byte(`{"action":"created","installation":{"id":99},"repositories":[{"id":1,"name":"one","full_name":"acme/one"}]}`)
	inbox := &recordingInbox{inserted: true}
	handler, err := webhook.NewHandler([]byte(secret), inbox, nil)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := signedRequest(body, secret)
	request.Header.Set("X-GitHub-Delivery", validDeliveryID())
	request.Header.Set("X-GitHub-Event", "installation")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || inbox.calls != 1 {
		t.Fatalf("provisioning webhook = status %d, writes %d; want 202, 1", response.Code, inbox.calls)
	}
	delivery := inbox.deliveries[0]
	if delivery.EventName != "installation" || delivery.Action != "created" || string(delivery.Payload) != string(body) {
		t.Errorf("delivery = %#v, want installation.created with raw payload", delivery)
	}
}

func TestHandlerAcceptsRepositoryProvisioningPayload(t *testing.T) {
	const secret = "webhook-secret"
	body := []byte(`{"action":"added","installation":{"id":99},"repositories_added":[{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex"}]}`)
	inbox := &recordingInbox{inserted: true}
	handler, err := webhook.NewHandler([]byte(secret), inbox, nil)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := signedRequest(body, secret)
	request.Header.Set("X-GitHub-Delivery", validDeliveryID())
	request.Header.Set("X-GitHub-Event", "installation_repositories")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || inbox.calls != 1 {
		t.Fatalf("provisioning webhook = status %d, writes %d; want 202, 1", response.Code, inbox.calls)
	}
	if inbox.deliveries[0].Action != "added" {
		t.Errorf("action = %q, want added", inbox.deliveries[0].Action)
	}
}
