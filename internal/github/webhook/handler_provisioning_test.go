package webhook_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHandlerDurablyAcceptsLargeSignedRepositoryAddition(t *testing.T) {
	const secret = "webhook-secret"
	var payload strings.Builder
	payload.WriteString(`{"action":"added","installation":{"id":99},"repositories_added":[`)
	for id := 1; id <= 10_000; id++ {
		if id > 1 {
			payload.WriteByte(',')
		}
		name := fmt.Sprintf("repo-%05d-%s", id, strings.Repeat("x", 85))
		_, _ = fmt.Fprintf(&payload, `{"id":%d,"name":%q,"full_name":%q}`, id, name, "acme/"+name)
	}
	payload.WriteString(`]}`)
	body := []byte(payload.String())
	if len(body) <= 2<<20 {
		t.Fatalf("bulk addition is only %d bytes, want a body above the previous 2 MiB bound", len(body))
	}
	inbox := &recordingInbox{inserted: true}
	handler, err := webhook.NewHandler([]byte(secret), inbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRequest(body, secret)
	request.Header.Set("X-GitHub-Delivery", validDeliveryID())
	request.Header.Set("X-GitHub-Event", "installation_repositories")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || inbox.calls != 1 {
		t.Fatalf("bulk webhook = status %d, writes %d; want 202, 1", response.Code, inbox.calls)
	}
	if len(inbox.deliveries[0].Payload) != len(body) {
		t.Errorf("stored body = %d bytes, want %d", len(inbox.deliveries[0].Payload), len(body))
	}
}

func TestHandlerRejectsLargeProvisioningDeliveryWithInvalidSignature(t *testing.T) {
	const secret = "webhook-secret"
	inbox := &recordingInbox{}
	handler, err := webhook.NewHandler([]byte(secret), inbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRequest([]byte(strings.Repeat("x", 2<<20+1)), "wrong-secret")
	request.Header.Set("X-GitHub-Delivery", validDeliveryID())
	request.Header.Set("X-GitHub-Event", "installation_repositories")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || inbox.calls != 0 {
		t.Errorf("invalid large webhook = status %d, writes %d; want 401, 0", response.Code, inbox.calls)
	}
}
