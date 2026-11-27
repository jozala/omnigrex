package retention

import (
	"context"
	"errors"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

const NativeGCDependency = "docs/dependencies/microsandbox-safe-image-gc.md"

var ErrNativeApplyUnsupported = errors.New("native image apply refused: complete reachability and native exclusion are unqualified; see " + NativeGCDependency)

type ImageRoot struct {
	Image  string `json:"image"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}
type InventoryDimension struct {
	Category string `json:"category"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}
type NativeInventory struct {
	Backend        execution.BackendBinding `json:"backend"`
	Complete       bool                     `json:"complete"`
	ApplySupported bool                     `json:"apply_supported"`
	Roots          []ImageRoot              `json:"roots"`
	Dimensions     []InventoryDimension     `json:"dimensions"`
	Dependency     string                   `json:"dependency"`
}

// NativeImageInventory deliberately makes no removable-candidate inference from
// ledger absence. Native creators (including external clients) do not participate
// in a qualified GC exclusion contract in the pinned stack.
func NativeImageInventory(ctx context.Context, binding execution.BackendBinding, refs []execution.StateRef, images []string, limit int, truncated bool) (NativeInventory, error) {
	if limit < 1 || limit > 10000 {
		return NativeInventory{}, execution.ErrInvalidSpec
	}
	report := NativeInventory{Backend: binding, Dependency: NativeGCDependency, Roots: []ImageRoot{}}
	seen := map[string]bool{}
	add := func(image, reason string) {
		if image == "" || seen[image] {
			return
		}
		seen[image] = true
		if len(report.Roots) >= limit {
			truncated = true
			return
		}
		report.Roots = append(report.Roots, ImageRoot{Image: image, Status: "protected", Reason: reason})
	}
	for _, image := range images {
		add(image, "deployment or explicitly pinned/provisioned image")
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if ref.Backend != binding {
			return report, execution.ErrBindingMismatch
		}
		add(ref.Image, "uncollected Participant; includes COLLECTING generations")
	}
	status, reason := "observed", "bounded durable uncollected Participant image roots"
	if truncated {
		status, reason = "incomplete", "inventory limit reached; omitted roots remain protected"
	}
	report.Dimensions = append(report.Dimensions, InventoryDimension{"ledger-and-explicit-roots", status, reason})
	for _, category := range []string{"native-images-and-flattened-artifacts", "shared-layers-and-index-platform-aliases", "native-sandboxes-and-snapshots", "in-flight-pulls-materializations-and-executions", "external-client-and-provisioning-roots"} {
		report.Dimensions = append(report.Dimensions, InventoryDimension{category, "unsupported", "complete bounded provenance and native exclusion unavailable; retain all resources"})
	}
	return report, nil
}
