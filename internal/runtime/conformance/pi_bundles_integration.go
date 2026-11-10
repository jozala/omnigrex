//go:build integration

package conformance

import (
	"context"
	"encoding/json"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

// PiBundleInventories qualifies authentic R1/R2 image contents without creating
// a production multi-version admission catalogue or changing the selection.
func PiBundleInventories(t *testing.T, ctx context.Context, r1, r2 profile.Profile, check func(context.Context, profile.Profile) error) {
	t.Helper()
	// Schema versions alone do not make two runtime contents different.
	var first, second piresources.ResourceContract
	if err := json.Unmarshal(r1.Contract().GoldenResourceContract, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r2.Contract().GoldenResourceContract, &second); err != nil {
		t.Fatal(err)
	}
	equivalent := first.NodeVersion == second.NodeVersion && first.MiseSHA256 == second.MiseSHA256 && reflect.DeepEqual(first.Files, second.Files)
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
	if err := check(ctx, incompatible); (err == nil) != equivalent {
		t.Fatalf("cross-inventory qualification (equivalent=%t): %v", equivalent, err)
	}
	// Conversely a published future candidate is not automatically the selection.
	c = r1.Contract()
	c.Image = r2.Binding().Image
	unselected, err := profile.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(ctx, unselected); (err == nil) != equivalent {
		t.Fatalf("reverse cross-inventory qualification (equivalent=%t): %v", equivalent, err)
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
