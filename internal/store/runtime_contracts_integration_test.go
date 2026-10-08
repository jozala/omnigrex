//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestRepositoryRuntimeContractIsCommittedAtomicallyAndImmutable(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	triggerPreparationWorkflow(t, database, ctx, compatibilityUUID(61, 701), compatibilityUUID(62, 701))
	lease := claimPreparationJob(t, database, ctx)
	bound, err := compatibilityProfile(t, "b", "amd64").WithEnvironment(compatibilityImage("c"), profile.Platform{OS: "linux", Arch: "amd64"}, []string{"tool-installation"})
	if err != nil {
		t.Fatal(err)
	}
	spec := compatibilityPreparationSpec("repository-default-snapshot", bound)
	spec.Developer.RuntimeContract, _ = json.Marshal(bound.Contract())
	tampered := spec
	tampered.Developer.RuntimeContract = json.RawMessage(`{"name":"invalid"}`)
	if _, err := database.PrepareAgentTurn(ctx, lease, tampered); err == nil {
		t.Fatal("invalid contract was committed")
	}
	if _, err := database.RuntimeProfileContract(ctx, bound.Binding()); !errors.Is(err, profile.ErrNotFound) {
		t.Fatalf("failed preparation retained contract: %v", err)
	}
	prepared, err := database.PrepareAgentTurn(ctx, lease, spec)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Participant.RuntimeImageDigest != bound.Contract().Image {
		t.Fatal("Participant lost repository image")
	}
	content, err := database.RuntimeProfileContract(ctx, bound.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := profile.DecodeContract(content, bound.Binding()); err != nil || resolved.Binding() != bound.Binding() {
		t.Fatalf("stored immutable resolution: %+v %v", resolved.Contract(), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runtime_profile_contracts SET contract = '{}' WHERE content_sha256 = $1`, bound.ContentSHA256()); err == nil {
		t.Fatal("persisted contract was mutable")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runtime_profile_contracts WHERE content_sha256 = $1`, bound.ContentSHA256()); err == nil {
		t.Fatal("persisted contract was removable")
	}
	// A second fresh Participant of the same Role can select another image
	// without cross-image state migration or a historical Session upgrade.
	delivery := workflowDelivery(compatibilityUUID(61, 702))
	delivery.IssueID++
	delivery.IssueNumber++
	claim := claimWorkflowDelivery(t, database, ctx, delivery)
	locator := workflowLocator()
	locator.IssueID = delivery.IssueID
	locator.IssueNumber = delivery.IssueNumber
	if _, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"), locator, func(event store.WorkflowEventContext) (workflow.Event, error) {
			return workflow.TriggerEvent{EventMetadata: event.Metadata, AttemptID: compatibilityUUID(62, 702), AttemptNumber: event.Snapshot.LastAttemptNumber + 1}, nil
		}); err != nil {
		t.Fatal(err)
	}
	secondLease := claimPreparationJob(t, database, ctx)
	newImage, err := bound.WithEnvironment(compatibilityImage("d"), bound.Contract().Platform, []string{"tool-installation"})
	if err != nil {
		t.Fatal(err)
	}
	secondSpec := compatibilityPreparationSpec("new-repository-snapshot", newImage)
	secondSpec.Developer.RuntimeContract, _ = json.Marshal(newImage.Contract())
	if err := database.CheckPendingRuntimeProfileCompatibility(ctx, newImage); err != nil {
		t.Fatalf("fresh Participant admission required migration: %v", err)
	}
	second, err := database.PrepareAgentTurn(ctx, secondLease, secondSpec)
	if err != nil {
		t.Fatalf("independent repository image admission: %v", err)
	}
	if second.Participant.ID == prepared.Participant.ID || second.Session.ID == prepared.Session.ID || second.Participant.RuntimeImageDigest != newImage.Contract().Image {
		t.Fatal("new image reused an existing Participant or Session")
	}
	var originalImage string
	if err := pool.QueryRow(ctx, `SELECT runtime_image_digest FROM agent_assignments WHERE id = $1`, prepared.Participant.ID).Scan(&originalImage); err != nil || originalImage != bound.Contract().Image {
		t.Fatalf("original Participant image changed: %s %v", originalImage, err)
	}
}
