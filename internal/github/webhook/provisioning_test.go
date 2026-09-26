package webhook_test

import (
	"errors"
	"testing"

	"github.com/jozala/omnigrex/internal/github/webhook"
)

func TestNormalizeInstallationCreatedRequiresEnumeration(t *testing.T) {
	delivery := webhook.Delivery{
		DeliveryID: validDeliveryID(),
		EventName:  "installation",
		Action:     "created",
		Payload: []byte(`{"action":"created","installation":{"id":99},
			"repositories":[{"id":1,"name":"one","full_name":"acme/one"}]}`),
	}

	result, err := webhook.Normalize(delivery)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if result.Outcome != webhook.NormalizationProvisioning || result.Provisioning == nil {
		t.Fatalf("result = %#v, want provisioning outcome", result)
	}
	provisioning := result.Provisioning
	if provisioning.InstallationID != 99 {
		t.Errorf("installation ID = %d, want 99", provisioning.InstallationID)
	}
	if len(provisioning.Repositories) != 0 {
		t.Errorf("repositories = %#v, want empty for enumerated installation creation", provisioning.Repositories)
	}
	if result.Event != nil {
		t.Errorf("workflow event = %#v, want nil for provisioning", result.Event)
	}
}

func TestNormalizeInstallationRepositoriesAdded(t *testing.T) {
	delivery := webhook.Delivery{
		DeliveryID: validDeliveryID(),
		EventName:  "installation_repositories",
		Action:     "added",
		Payload: []byte(`{"action":"added","installation":{"id":99},
			"repositories_added":[
				{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex"},
				{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex"},
				{"id":9124,"name":"widgets","full_name":"jozala/widgets"}
			]}`),
	}

	result, err := webhook.Normalize(delivery)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if result.Outcome != webhook.NormalizationProvisioning || result.Provisioning == nil {
		t.Fatalf("result = %#v, want provisioning outcome", result)
	}
	repositories := result.Provisioning.Repositories
	if len(repositories) != 2 || repositories[0].ID != 9123 || repositories[0].Owner != "jozala" || repositories[0].Name != "omnigrex" ||
		repositories[1].ID != 9124 || repositories[1].Name != "widgets" {
		t.Errorf("repositories = %#v, want deduplicated added repositories", repositories)
	}
}

func TestNormalizeRepositoryCreated(t *testing.T) {
	delivery := webhook.Delivery{
		DeliveryID: validDeliveryID(),
		EventName:  "repository",
		Action:     "created",
		Payload: []byte(`{"action":"created","installation":{"id":99},
			"repository":{"id":9123,"name":"omnigrex","full_name":"jozala/omnigrex","owner":{"login":"jozala"}}}`),
	}

	result, err := webhook.Normalize(delivery)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if result.Outcome != webhook.NormalizationProvisioning || result.Provisioning == nil {
		t.Fatalf("result = %#v, want provisioning outcome", result)
	}
	repositories := result.Provisioning.Repositories
	if len(repositories) != 1 || repositories[0].ID != 9123 || repositories[0].Owner != "jozala" || repositories[0].Name != "omnigrex" {
		t.Errorf("repositories = %#v, want created repository identity", repositories)
	}
}

func TestNormalizeIgnoresProvisioningRemovals(t *testing.T) {
	tests := []webhook.Delivery{
		{DeliveryID: validDeliveryID(), EventName: "installation", Action: "deleted", Payload: []byte(`{"action":"deleted","installation":{"id":99}}`)},
		{DeliveryID: validDeliveryID(), EventName: "installation_repositories", Action: "removed", Payload: []byte(`{"action":"removed","installation":{"id":99}}`)},
		{DeliveryID: validDeliveryID(), EventName: "repository", Action: "deleted", Payload: []byte(`{"action":"deleted","installation":{"id":99}}`)},
	}
	for _, delivery := range tests {
		result, err := webhook.Normalize(delivery)
		if err != nil {
			t.Errorf("Normalize(%s.%s) error = %v", delivery.EventName, delivery.Action, err)
			continue
		}
		if result.Outcome != webhook.NormalizationIgnored || result.Provisioning != nil || result.Event != nil {
			t.Errorf("Normalize(%s.%s) = %#v, want ignored", delivery.EventName, delivery.Action, result)
		}
	}
}

func TestNormalizeRejectsMalformedProvisioning(t *testing.T) {
	tests := []webhook.Delivery{
		{DeliveryID: validDeliveryID(), EventName: "installation", Action: "created", Payload: []byte(`{"action":"created"}`)},
		{DeliveryID: validDeliveryID(), EventName: "installation", Action: "", Payload: []byte(`{"installation":{"id":99}}`)},
		{DeliveryID: validDeliveryID(), EventName: "installation_repositories", Action: "added", Payload: []byte(`{"action":"added","installation":{"id":99},"repositories_added":[{"id":0,"name":"","full_name":""}]}`)},
		{DeliveryID: validDeliveryID(), EventName: "installation_repositories", Action: "added", Payload: []byte(`{"action":"added","installation":{"id":99},"repositories_added":[{"id":9123,"name":"omnigrex","full_name":"wrong/name"}]}`)},
		{DeliveryID: validDeliveryID(), EventName: "repository", Action: "created", Payload: []byte(`{"action":"created","installation":{"id":99},"repository":{"id":9123,"name":"omnigrex","owner":{"login":""}}}`)},
		{DeliveryID: validDeliveryID(), EventName: "repository", Action: "created", Payload: []byte(`{"action":"added","installation":{"id":99},"repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}}}`)},
		{DeliveryID: "not-a-uuid", EventName: "installation", Action: "created", Payload: []byte(`{"action":"created","installation":{"id":99}}`)},
	}
	for _, delivery := range tests {
		if _, err := webhook.Normalize(delivery); !errors.Is(err, webhook.ErrMalformedPayload) {
			t.Errorf("Normalize(%s.%s) error = %v, want ErrMalformedPayload", delivery.EventName, delivery.Action, err)
		}
	}
}
