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
	if err := database.CompleteLabelProvisioningTransition(ctx, claim.DeliveryID, claim.ClaimToken, 99, repositories); err != nil {
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

	first, err := database.ClaimLabelProvisioningJob(ctx, "provisioner", 30*time.Second)
	if err != nil || first == nil {
		t.Fatalf("first ClaimLabelProvisioningJob() = (%#v, %v), want lease", first, err)
	}
	second, err := database.ClaimLabelProvisioningJob(ctx, "provisioner", 30*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second ClaimLabelProvisioningJob() = (%#v, %v), want second repository", second, err)
	}
	if first.RepositoryID == second.RepositoryID {
		t.Errorf("claimed repository IDs = (%d, %d), want distinct repositories", first.RepositoryID, second.RepositoryID)
	}
	if first.InstallationID != 99 || first.SourceDeliveryID != deliveryID {
		t.Errorf("first job = (%d, %q), want installation 99 from delivery %s", first.InstallationID, first.SourceDeliveryID, deliveryID)
	}
	if err := database.CompleteLabelProvisioningJob(ctx, *first, json.RawMessage(`{"provisioned":true}`)); err != nil {
		t.Fatalf("CompleteLabelProvisioningJob() error = %v", err)
	}
	if err := database.FailLabelProvisioningJob(ctx, *second, errProvisioningBoom, true, 0); err != nil {
		t.Fatalf("FailLabelProvisioningJob() error = %v", err)
	}
	retry, err := database.ClaimLabelProvisioningJob(ctx, "provisioner", 30*time.Second)
	if err != nil || retry == nil || retry.RepositoryID != second.RepositoryID || retry.Attempt != 2 {
		t.Errorf("retry ClaimLabelProvisioningJob() = (%#v, %v), want failed repository attempt 2", retry, err)
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
	}); err != nil {
		t.Fatalf("CompleteLabelProvisioningTransition() error = %v", err)
	}
	first, err := database.ClaimLabelProvisioningJob(ctx, "provisioner", 30*time.Second)
	if err != nil || first == nil {
		t.Fatalf("ClaimLabelProvisioningJob() = (%#v, %v)", first, err)
	}
	if err := database.FailLabelProvisioningJob(ctx, *first, errProvisioningBoom, false, 0); err != nil {
		t.Fatalf("FailLabelProvisioningJob() error = %v", err)
	}
	second, err := database.ClaimLabelProvisioningJob(ctx, "provisioner", 30*time.Second)
	if err != nil || second == nil {
		t.Fatalf("second ClaimLabelProvisioningJob() = (%#v, %v), want unaffected repository", second, err)
	}
	if second.RepositoryID == first.RepositoryID {
		t.Errorf("second repository = %d, want the other repository", second.RepositoryID)
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
