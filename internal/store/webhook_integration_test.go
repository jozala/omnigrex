//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

func TestInsertWebhookDeliveryDeduplicatesDeliveryID(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivery := store.WebhookDelivery{
		DeliveryID:      "123e4567-e89b-12d3-a456-426614174000",
		EventName:       "issues",
		Action:          "labeled",
		RepositoryID:    9123,
		RepositoryOwner: "jozala",
		RepositoryName:  "omnigrex",
		IssueID:         456,
		IssueNumber:     12,
		Headers:         map[string]string{"X-GitHub-Event": "issues"},
		Payload:         []byte(`{"original":true}`),
	}

	inserted, err := database.InsertWebhookDelivery(ctx, delivery)
	if err != nil {
		t.Fatalf("first InsertWebhookDelivery() error = %v", err)
	}
	if !inserted {
		t.Error("first InsertWebhookDelivery() inserted = false, want true")
	}
	delivery.Payload = []byte(`{"replacement":true}`)
	inserted, err = database.InsertWebhookDelivery(ctx, delivery)
	if err != nil {
		t.Fatalf("duplicate InsertWebhookDelivery() error = %v", err)
	}
	if inserted {
		t.Error("duplicate InsertWebhookDelivery() inserted = true, want false")
	}

	got, err := database.GetWebhookDelivery(ctx, delivery.DeliveryID)
	if err != nil {
		t.Fatalf("GetWebhookDelivery() error = %v", err)
	}
	if got.Status != store.WebhookPending || got.AttemptCount != 0 {
		t.Errorf("delivery state = (%q, %d attempts), want (PENDING, 0 attempts)", got.Status, got.AttemptCount)
	}
	if got.IssueID != 456 || got.IssueNumber != 12 {
		t.Errorf("stored Issue identity = (%d, %d), want (456, 12)", got.IssueID, got.IssueNumber)
	}
	if string(got.Payload) != `{"original":true}` {
		t.Errorf("stored payload = %s, want original payload", got.Payload)
	}
}

func TestClaimWebhookDeliveryReclaimsExpiredLeaseAndFencesOldOwner(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivery := webhookDelivery("123e4567-e89b-12d3-a456-426614174000")
	if _, err := database.InsertWebhookDelivery(ctx, delivery); err != nil {
		t.Fatalf("InsertWebhookDelivery() error = %v", err)
	}

	first, err := database.ClaimWebhookDelivery(ctx, "processor-a", 75*time.Millisecond)
	if err != nil {
		t.Fatalf("first ClaimWebhookDelivery() error = %v", err)
	}
	if first == nil {
		t.Fatal("first ClaimWebhookDelivery() = nil, want claim")
	}
	if first.AttemptCount != 1 || first.ClaimOwner != "processor-a" || first.ClaimToken == "" {
		t.Errorf("first claim = %#v, want attempt 1 owned by processor-a with token", first)
	}
	claimedAgain, err := database.ClaimWebhookDelivery(ctx, "processor-b", time.Second)
	if err != nil {
		t.Fatalf("ClaimWebhookDelivery() during live lease error = %v", err)
	}
	if claimedAgain != nil {
		t.Errorf("ClaimWebhookDelivery() during live lease = %#v, want nil", claimedAgain)
	}

	time.Sleep(100 * time.Millisecond)
	second, err := database.ClaimWebhookDelivery(ctx, "processor-b", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("reclaim ClaimWebhookDelivery() error = %v", err)
	}
	if second == nil {
		t.Fatal("reclaim ClaimWebhookDelivery() = nil, want expired delivery")
	}
	if second.AttemptCount != 2 || second.ClaimOwner != "processor-b" || second.ClaimToken == first.ClaimToken {
		t.Errorf("reclaimed delivery = %#v, want attempt 2 with a new processor-b token", second)
	}

	err = database.RenewWebhookClaim(ctx, delivery.DeliveryID, first.ClaimToken, time.Second)
	if !errors.Is(err, store.ErrWebhookClaimLost) {
		t.Errorf("RenewWebhookClaim() with old token error = %v, want ErrWebhookClaimLost", err)
	}
	beforeRenewal := second.LeaseExpiresAt
	if err := database.RenewWebhookClaim(ctx, delivery.DeliveryID, second.ClaimToken, time.Second); err != nil {
		t.Fatalf("RenewWebhookClaim() with current token error = %v", err)
	}
	got, err := database.GetWebhookDelivery(ctx, delivery.DeliveryID)
	if err != nil {
		t.Fatalf("GetWebhookDelivery() error = %v", err)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.After(beforeRenewal) {
		t.Errorf("renewed lease expiry = %v, want after %v", got.LeaseExpiresAt, beforeRenewal)
	}
}

func TestClaimWebhookDeliveryAllowsConcurrentProcessorsToSkipLockedRows(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, deliveryID := range []string{
		"123e4567-e89b-12d3-a456-426614174000",
		"223e4567-e89b-12d3-a456-426614174000",
	} {
		if _, err := database.InsertWebhookDelivery(ctx, webhookDelivery(deliveryID)); err != nil {
			t.Fatalf("insert delivery %s: %v", deliveryID, err)
		}
	}

	start := make(chan struct{})
	results := make(chan *store.WebhookClaim, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for _, owner := range []string{"processor-a", "processor-b"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claim, err := database.ClaimWebhookDelivery(ctx, owner, 30*time.Second)
			results <- claim
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent ClaimWebhookDelivery() error = %v", err)
		}
	}
	claimedIDs := make(map[string]bool)
	for claim := range results {
		if claim == nil {
			t.Fatal("concurrent ClaimWebhookDelivery() = nil, want one claim per processor")
		}
		if claim.AttemptCount != 1 {
			t.Errorf("claim attempt count = %d, want 1", claim.AttemptCount)
		}
		if claimedIDs[claim.DeliveryID] {
			t.Errorf("delivery %s claimed by both processors", claim.DeliveryID)
		}
		claimedIDs[claim.DeliveryID] = true
	}
	if len(claimedIDs) != 2 {
		t.Errorf("claimed delivery IDs = %#v, want two distinct rows", claimedIDs)
	}
}

func TestCompleteWebhookDeliveryAtomicallyRecordsSupportedOrIgnoredOutcome(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	supported := webhookDelivery("123e4567-e89b-12d3-a456-426614174000")
	if _, err := database.InsertWebhookDelivery(ctx, supported); err != nil {
		t.Fatalf("insert supported delivery: %v", err)
	}
	claim, err := database.ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim supported delivery = (%#v, %v), want claim", claim, err)
	}
	payload := json.RawMessage(`{"delivery_id":"123e4567-e89b-12d3-a456-426614174000","event":"issues","action":"closed"}`)
	if err := database.CompleteWebhookDelivery(ctx, claim.DeliveryID, "323e4567-e89b-12d3-a456-426614174000", store.WebhookCompletion{
		Outcome:           store.WebhookOutcomeProcessed,
		NormalizedPayload: payload,
	}); !errors.Is(err, store.ErrWebhookClaimLost) {
		t.Errorf("CompleteWebhookDelivery() with wrong token error = %v, want ErrWebhookClaimLost", err)
	}
	if _, err := database.GetNormalizedEvent(ctx, supported.DeliveryID); !errors.Is(err, store.ErrNormalizedEventNotFound) {
		t.Errorf("GetNormalizedEvent() before fenced completion error = %v, want ErrNormalizedEventNotFound", err)
	}
	if err := database.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{
		Outcome:           store.WebhookOutcomeProcessed,
		NormalizedPayload: payload,
	}); err != nil {
		t.Fatalf("CompleteWebhookDelivery(PROCESSED) error = %v", err)
	}

	deliveryState, err := database.GetWebhookDelivery(ctx, supported.DeliveryID)
	if err != nil {
		t.Fatalf("get processed delivery: %v", err)
	}
	if deliveryState.Status != store.WebhookProcessed || deliveryState.ProcessedAt == nil || deliveryState.ClaimToken != nil {
		t.Errorf("processed delivery state = %#v, want completed and unclaimed", deliveryState)
	}
	event, err := database.GetNormalizedEvent(ctx, supported.DeliveryID)
	if err != nil {
		t.Fatalf("GetNormalizedEvent() error = %v", err)
	}
	if event.Status != store.NormalizedEventPending || !json.Valid(event.Payload) {
		t.Errorf("normalized event = %#v, want valid PENDING event", event)
	}
	if err := database.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{
		Outcome:           store.WebhookOutcomeProcessed,
		NormalizedPayload: payload,
	}); !errors.Is(err, store.ErrWebhookClaimLost) {
		t.Errorf("duplicate CompleteWebhookDelivery() error = %v, want ErrWebhookClaimLost", err)
	}

	ignored := webhookDelivery("223e4567-e89b-12d3-a456-426614174000")
	ignored.EventName = "push"
	ignored.Action = ""
	if _, err := database.InsertWebhookDelivery(ctx, ignored); err != nil {
		t.Fatalf("insert ignored delivery: %v", err)
	}
	claim, err = database.ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim ignored delivery = (%#v, %v), want claim", claim, err)
	}
	if err := database.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, store.WebhookCompletion{
		Outcome: store.WebhookOutcomeIgnored,
	}); err != nil {
		t.Fatalf("CompleteWebhookDelivery(IGNORED) error = %v", err)
	}
	deliveryState, err = database.GetWebhookDelivery(ctx, ignored.DeliveryID)
	if err != nil {
		t.Fatalf("get ignored delivery: %v", err)
	}
	if deliveryState.Status != store.WebhookIgnored || deliveryState.ProcessedAt == nil {
		t.Errorf("ignored delivery state = %#v, want IGNORED completion", deliveryState)
	}
	if _, err := database.GetNormalizedEvent(ctx, ignored.DeliveryID); !errors.Is(err, store.ErrNormalizedEventNotFound) {
		t.Errorf("GetNormalizedEvent() for ignored delivery error = %v, want ErrNormalizedEventNotFound", err)
	}
}

func TestFailWebhookDeliveryIsFencedAndRecordsFailure(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivery := webhookDelivery("123e4567-e89b-12d3-a456-426614174000")
	if _, err := database.InsertWebhookDelivery(ctx, delivery); err != nil {
		t.Fatalf("InsertWebhookDelivery() error = %v", err)
	}
	claim, err := database.ClaimWebhookDelivery(ctx, "processor", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}

	err = database.FailWebhookDelivery(ctx, claim.DeliveryID, "323e4567-e89b-12d3-a456-426614174000", errors.New("wrong shape"))
	if !errors.Is(err, store.ErrWebhookClaimLost) {
		t.Errorf("FailWebhookDelivery() with wrong token error = %v, want ErrWebhookClaimLost", err)
	}
	if err := database.FailWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, errors.New("wrong shape")); err != nil {
		t.Fatalf("FailWebhookDelivery() error = %v", err)
	}
	record, err := database.GetWebhookDelivery(ctx, delivery.DeliveryID)
	if err != nil {
		t.Fatalf("GetWebhookDelivery() error = %v", err)
	}
	if record.Status != store.WebhookFailed || record.ProcessedAt == nil || record.ClaimToken != nil || record.LastError == nil || *record.LastError != "wrong shape" {
		t.Errorf("failed delivery state = %#v, want FAILED, unclaimed, with wrong shape error", record)
	}
	if _, err := database.GetNormalizedEvent(ctx, delivery.DeliveryID); !errors.Is(err, store.ErrNormalizedEventNotFound) {
		t.Errorf("GetNormalizedEvent() after failure error = %v, want ErrNormalizedEventNotFound", err)
	}
}

func TestExpiredWebhookClaimCannotCompleteOrFail(t *testing.T) {
	database := openWebhookStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivery := webhookDelivery("123e4567-e89b-12d3-a456-426614174000")
	if _, err := database.InsertWebhookDelivery(ctx, delivery); err != nil {
		t.Fatalf("InsertWebhookDelivery() error = %v", err)
	}
	claim, err := database.ClaimWebhookDelivery(ctx, "processor", 50*time.Millisecond)
	if err != nil || claim == nil {
		t.Fatalf("ClaimWebhookDelivery() = (%#v, %v), want claim", claim, err)
	}
	time.Sleep(75 * time.Millisecond)

	completion := store.WebhookCompletion{Outcome: store.WebhookOutcomeIgnored}
	if err := database.CompleteWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, completion); !errors.Is(err, store.ErrWebhookClaimLost) {
		t.Errorf("CompleteWebhookDelivery() after expiry error = %v, want ErrWebhookClaimLost", err)
	}
	if err := database.FailWebhookDelivery(ctx, claim.DeliveryID, claim.ClaimToken, errors.New("late failure")); !errors.Is(err, store.ErrWebhookClaimLost) {
		t.Errorf("FailWebhookDelivery() after expiry error = %v, want ErrWebhookClaimLost", err)
	}
}

func openWebhookStore(t *testing.T) *store.Store {
	t.Helper()
	postgres := startPostgres(t)
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatalf("write database password: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

func webhookDelivery(deliveryID string) store.WebhookDelivery {
	return store.WebhookDelivery{
		DeliveryID:      deliveryID,
		EventName:       "issues",
		Action:          "closed",
		RepositoryID:    9123,
		RepositoryOwner: "jozala",
		RepositoryName:  "omnigrex",
		IssueID:         456,
		IssueNumber:     12,
		Headers:         map[string]string{"X-GitHub-Event": "issues"},
		Payload:         []byte(`{"action":"closed"}`),
	}
}
