//go:build integration

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

// PiBundleInventories qualifies authentic R1/R2 image contents without creating
// a production multi-version admission catalogue or changing the selection.
func PiBundleInventories(t *testing.T, ctx context.Context, r1, r2 profile.Profile, check func(context.Context, profile.Profile) error) {
	t.Helper()
	if bytes.Equal(r1.Contract().GoldenResourceContract, r2.Contract().GoldenResourceContract) {
		t.Fatal("fixture must contain distinct captured resource inventories")
	}
	for _, p := range []profile.Profile{r1, r2} {
		if err := check(ctx, p); err != nil {
			t.Fatalf("authentic bundle did not qualify: %v", err)
		}
	}
	// Selecting R2 requires R2 resources even if an old image claims the same
	// protocol/package version. Fresh R1 images cannot pass R2 qualification.
	c := r2.Contract()
	c.Image = r1.Binding().Image
	incompatible, err := profile.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(ctx, incompatible); err == nil {
		t.Fatal("R1 image passed R2 admission resources")
	}
	// Conversely a published future candidate is not automatically the selection.
	c = r1.Contract()
	c.Image = r2.Binding().Image
	unselected, err := profile.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(ctx, unselected); err == nil {
		t.Fatal("unselected runtime candidate passed R1 resources")
	}
	encoded, _ := json.Marshal(r1.Contract())
	retained, err := profile.DecodeContract(encoded, r1.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if retained.Binding() != r1.Binding() {
		t.Fatal("retained R1 rebound to R2")
	}
	if err := check(ctx, retained); err != nil {
		t.Fatalf("retained R1 requires current resources: %v", err)
	}
}
