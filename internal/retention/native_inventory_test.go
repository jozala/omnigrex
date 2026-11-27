package retention_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func TestNativeInventoryNeverTreatsMissingEvidenceAsCandidate(t *testing.T) {
	binding := execution.BackendBinding{Family: "microsandbox", Version: 1, Namespace: "test"}
	refs := []execution.StateRef{{Backend: binding, Image: "retained"}, {Backend: binding, Image: "collecting"}, {Backend: binding, Image: "retained"}}
	report, err := retention.NativeImageInventory(context.Background(), binding, refs, []string{"deployment", "provisioned", "pinned"}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || report.ApplySupported || len(report.Roots) != 5 || report.Dependency == "" {
		t.Fatalf("unsafe report: %+v", report)
	}
	for _, root := range report.Roots {
		if root.Status != "protected" {
			t.Fatalf("unsafe root: %+v", root)
		}
	}
	if len(report.Dimensions) != 6 {
		t.Fatalf("missing provenance: %+v", report.Dimensions)
	}
	for _, dimension := range report.Dimensions[1:] {
		if dimension.Status != "unsupported" || !strings.Contains(dimension.Reason, "retain all") {
			t.Fatal(dimension)
		}
	}
	report, err = retention.NativeImageInventory(context.Background(), binding, refs, nil, 1, true)
	if err != nil || len(report.Roots) != 1 || report.Dimensions[0].Status != "incomplete" {
		t.Fatalf("limit: %+v, %v", report, err)
	}
	refs[0].Backend.Namespace = "wrong"
	if _, err := retention.NativeImageInventory(context.Background(), binding, refs, nil, 10, false); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatal(err)
	}
}
