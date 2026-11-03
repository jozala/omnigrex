package mcp

import (
	"github.com/jozala/omnigrex/internal/role"
	"testing"
)

func TestPlannerToolDiscoveryPreservesOtherRoleSchemas(t *testing.T) {
	policies, err := role.NewBuiltinPolicyCatalog([]role.ID{role.Planner, role.Developer})
	if err != nil {
		t.Fatal(err)
	}
	for _, roleID := range []role.ID{role.Planner, role.Developer, role.Planner, role.Developer} {
		tools, err := toolsForScope(policies, TokenScope{Role: roleID})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range tools {
			if roleID == role.Planner {
				switch tool.Name {
				case ToolPublishChanges, ToolOpenPR, ToolRequestReview, ToolSubmitReview:
					t.Fatalf("Planner exposed implementation tool %s", tool.Name)
				}
			}
			if tool.Name != ToolRequestClarification {
				continue
			}
			found = true
			questions := tool.InputSchema["properties"].(map[string]any)["questions"].(map[string]any)
			want := 50
			if roleID == role.Planner {
				want = 5
			}
			if questions["maxItems"] != want {
				t.Fatalf("%s question cap = %v, want %d", roleID, questions["maxItems"], want)
			}
		}
		if !found {
			t.Fatal("clarification unavailable")
		}
	}
}
