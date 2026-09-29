//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

var errProvisioningBoom = errors.New("provisioning boom")

func TestCompleteLabelProvisioningTransitionDedupesAndMarksDelivery(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deliveryID := "123e4567-e89b-12d3-a456-426614174000"
	insertProvisioningDelivery(t, database, ctx, deliveryID)
	claim, err := database.ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	repositories := []store.LabelProvisioningRepository{
		{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		{ID: 9124, Owner: "jozala", Name: "widgets"},
	}
	if err := database.CompleteLabelProvisioningTransition(ctx, claim.DeliveryID, claim.ClaimToken, 99, repositories, nil); err != nil {
		t.Fatalf("CompleteLabelProvisioningTransition() error = %v", err)
	}

	record, err := database.GetWebhookDelivery(ctx, deliveryID)
	if err != nil {
		t.Fatalf("GetWebhookDelivery() error = %v", err)
	}
	if record.Status != store.WebhookProcessed {
		t.Errorf("delivery status = %q, want PROCESSED", record.Status)
	}
	if _, err := database.GetNormalizedEvent(ctx, deliveryID); err == nil {
		t.Errorf("GetNormalizedEvent() succeeded, want no Workflow event for provisioning")
	}

	first, err := database.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "provisioner", 30*time.Second)
	if err != nil || first == nil {
		t.Fatalf("first ClaimJobKind() = (%#v, %v), want lease", first, err)
	}
	second, err := database.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "provisioner", 30*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second ClaimJobKind() = (%#v, %v), want second repository", second, err)
	}
	firstPayload, err := store.ParseLabelProvisioningPayload(first.Payload)
	if err != nil {
		t.Fatalf("parse first payload: %v", err)
	}
	secondPayload, err := store.ParseLabelProvisioningPayload(second.Payload)
	if err != nil {
		t.Fatalf("parse second payload: %v", err)
	}
	if firstPayload.RepositoryID == secondPayload.RepositoryID {
		t.Errorf("claimed repository IDs = (%d, %d), want distinct repositories", firstPayload.RepositoryID, secondPayload.RepositoryID)
	}
	if firstPayload.InstallationID != 99 {
		t.Errorf("first installation ID = %d, want 99", firstPayload.InstallationID)
	}
	if first.IdempotencyKey == "" || first.IdempotencyKey == second.IdempotencyKey {
		t.Errorf("idempotency keys = (%q, %q), want distinct keys", first.IdempotencyKey, second.IdempotencyKey)
	}
	if err := database.CompleteJob(ctx, *first, json.RawMessage(`{"provisioned":true}`)); err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}
	if err := database.FailJob(ctx, *second, errProvisioningBoom, true, 0); err != nil {
		t.Fatalf("FailJob() error = %v", err)
	}
	retry, err := database.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "provisioner", 30*time.Second)
	if err != nil || retry == nil || retry.Attempt != 2 {
		t.Errorf("retry ClaimJobKind() = (%#v, %v), want failed repository attempt 2", retry, err)
	} else if retryPayload, err := store.ParseLabelProvisioningPayload(retry.Payload); err != nil || retryPayload.RepositoryID != secondPayload.RepositoryID {
		t.Errorf("retry payload = (%#v, %v), want repository %d", retryPayload, err, secondPayload.RepositoryID)
	}
}

func TestLabelProvisioningFailureIsolationAcrossRepositories(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deliveryID := "223e4567-e89b-12d3-a456-426614174000"
	insertProvisioningDelivery(t, database, ctx, deliveryID)
	claim, err := database.ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	if err := database.CompleteLabelProvisioningTransition(ctx, claim.DeliveryID, claim.ClaimToken, 99, []store.LabelProvisioningRepository{
		{ID: 9123, Owner: "jozala", Name: "omnigrex"},
		{ID: 9124, Owner: "jozala", Name: "widgets"},
	}, nil); err != nil {
		t.Fatalf("CompleteLabelProvisioningTransition() error = %v", err)
	}
	first, err := database.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "provisioner", 30*time.Second)
	if err != nil || first == nil {
		t.Fatalf("ClaimJobKind() = (%#v, %v)", first, err)
	}
	if err := database.FailJob(ctx, *first, errProvisioningBoom, false, 0); err != nil {
		t.Fatalf("FailJob() error = %v", err)
	}
	second, err := database.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "provisioner", 30*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second ClaimJobKind() = (%#v, %v), want unaffected repository", second, err)
	}
	firstPayload, _ := store.ParseLabelProvisioningPayload(first.Payload)
	secondPayload, _ := store.ParseLabelProvisioningPayload(second.Payload)
	if secondPayload.RepositoryID == firstPayload.RepositoryID {
		t.Errorf("second repository = %d, want the other repository", secondPayload.RepositoryID)
	}
}

func TestCompleteLabelProvisioningTransitionRecordsInvalidEntriesAsDeliveryFailure(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deliveryID := "323e4567-e89b-12d3-a456-426614174000"
	insertProvisioningDelivery(t, database, ctx, deliveryID)
	claim, err := database.ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	if err := database.CompleteLabelProvisioningTransition(ctx, claim.DeliveryID, claim.ClaimToken, 99,
		[]store.LabelProvisioningRepository{{ID: 9123, Owner: "jozala", Name: "omnigrex"}},
		[]string{"repositories_added[1] has an incomplete repository entry"}); err != nil {
		t.Fatalf("CompleteLabelProvisioningTransition() error = %v", err)
	}

	record, err := database.GetWebhookDelivery(ctx, deliveryID)
	if err != nil {
		t.Fatalf("GetWebhookDelivery() error = %v", err)
	}
	if record.Status != store.WebhookFailed {
		t.Errorf("delivery status = %q, want FAILED with queued jobs", record.Status)
	}
	if record.LastError == nil || *record.LastError == "" {
		t.Errorf("delivery last error = %v, want invalid entry diagnostic", record.LastError)
	}

	provisioned, err := database.ClaimJobKind(ctx, store.LabelProvisioningQueue, store.ProvisionManagedLabelsJobKind, "provisioner", 30*time.Second)
	if err != nil || provisioned == nil {
		t.Fatalf("ClaimJobKind() = (%#v, %v), want valid repository job despite invalid entries", provisioned, err)
	}
	if payload, err := store.ParseLabelProvisioningPayload(provisioned.Payload); err != nil || payload.RepositoryID != 9123 {
		t.Errorf("provisioned payload = (%#v, %v), want repository 9123", payload, err)
	}
}

func insertProvisioningDelivery(t *testing.T, database *store.Store, ctx context.Context, deliveryID string) {
	t.Helper()
	if _, err := database.InsertWebhookDelivery(ctx, store.WebhookDelivery{
		DeliveryID: deliveryID, EventName: "installation", Action: "created",
		Headers: map[string]string{"X-GitHub-Event": "installation"},
		Payload: []byte(`{"action":"created","installation":{"id":99}}`),
	}); err != nil {
		t.Fatalf("InsertWebhookDelivery() error = %v", err)
	}
}
