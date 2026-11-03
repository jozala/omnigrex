package agentinstructions_test

import (
	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/workflow"
	"strings"
	"testing"
)

func TestPlannerDiscoveryComesFromCurrentDeploymentWithoutRepositorySkill(t *testing.T) {
	operator, err := agentinstructions.Load("", role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatal(err)
	}
	policies, err := role.NewBuiltinPolicyCatalog([]role.ID{role.Planner})
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := policies.Lookup(role.Planner)
	stage := workflow.StageDefinition{ID: "planning", Role: policy.Role, Instructions: "Plan only."}
	// Composition is invoked on every launch, including retained Session continuation.
	for _, purpose := range []workflow.TurnPurpose{"INITIAL_PLANNING", workflow.TurnPurposeReactivation, workflow.TurnPurposeRetry} {
		current := operator.Compose(policy, stage, purpose, "Repository preferences only.")
		for _, want := range []string{"Omnigrex discovery procedure v1", "4ae2317ddb22b046e26455e4f9c9b3aaaf62212d", "at most five", "prerequisites", "Investigate repository facts", "unresolved", "confirmed human decisions", "requirements brief", "technical plan", "confirm-plan", "not implementation authorization", "no sub-agents", "Copyright (c) 2026 Matt Pocock", "Permission is hereby granted", "THE SOFTWARE IS PROVIDED"} {
			if !strings.Contains(current, want) {
				t.Errorf("%s instructions missing %q", purpose, want)
			}
		}
		if strings.Contains(current, "Load `grilling`") {
			t.Fatal("core procedure requires repository skill installation")
		}
	}
	developer, _ := role.BuiltinPolicyCatalog().Lookup(role.Developer)
	current := operator.Compose(developer, workflow.StageDefinition{ID: "implementation"}, workflow.TurnPurposeInitialDevelopment, "")
	if strings.Contains(current, "Omnigrex discovery procedure v1") {
		t.Fatal("Planner discovery leaked into Developer instructions")
	}
}
