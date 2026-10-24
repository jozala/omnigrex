//go:build integration

package pi_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/stepbudget"
)

func TestPiConfigurationAndRoleAuthority(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_PROTOCOL_INTEGRATION") != "1" {
		t.Skip("explicit both-backend Pi gate required")
	}
	image, platform, settings := diagnosticFixture(t)
	for _, tc := range []struct {
		name    string
		role    role.ID
		prompt  string
		corrupt string
	}{
		{"Developer", role.Developer, "", ""}, {"Reviewer", role.Reviewer, "", ""},
		{"Reviewer developer-only MCP", role.Reviewer, "configuration-developer-only", ""},
		{"Developer publication authority", role.Developer, "configuration-publish", ""},
		{"revoked Turn fence", role.Developer, "configuration-revoked", ""},
		{"exact MCP schema rejection", role.Developer, "configuration-invalid-arguments", ""},
		{"unsupported model", role.Developer, "", "model"}, {"unsupported thinking", role.Developer, "", "thinking"},
		{"invalid MCP token", role.Developer, "", "token"}, {"MCP authority mismatch", role.Reviewer, "", "authority"},
		{"skills disabled", role.Reviewer, "configuration-audit", "skills-off"},
		{"all local tools denied", role.Reviewer, "configuration-audit", "no-local-tools"},
		{"retained instruction refresh", role.Reviewer, "configuration-audit", "retained"},
		{"skill escape", role.Reviewer, "", "skill-escape"},
		{"MCP redirect boundary", role.Reviewer, "configuration-audit", "mcp-redirect"},
		{"Developer provider failure retention", role.Developer, "configuration-provider-error", ""},
		{"Reviewer provider failure retention", role.Reviewer, "configuration-provider-error", ""},
		{"failed partial response retention", role.Developer, "configuration-provider-stream-error", ""},
		{"failure record sanitation boundaries", role.Developer, "configuration-audit", "failure-boundary"},
		{"durable file safety boundaries", role.Developer, "configuration-audit", "session-boundary"},
		{"step budget SDK boundaries", role.Developer, "configuration-audit", "budget-boundary"},
		{"production budget 1", role.Developer, "configuration-budget", "budget-1"},
		{"production budget N", role.Developer, "configuration-budget", "budget-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			inputs, err := pi.ConfigurationDiagnostic(tc.role)
			if err != nil {
				t.Fatal(err)
			}
			spec := pi.ConfigurationDiagnosticSpec(image, platform)
			if settings.ExecutionBackend == "microsandbox" {
				spec.Namespace = settings.MicrosandboxNamespace
			} else {
				spec.Namespace = settings.RuntimeStateVolume
			}
			switch tc.corrupt {
			case "model":
				inputs.Selection.Model = "diagnostic/does-not-exist"
			case "thinking":
				inputs.Selection.Thinking = "high"
			case "token":
				inputs.MCP.Headers[0].Value = "Bearer invalid-authority"
			case "authority":
				inputs.MCPTools = append(inputs.MCPTools, "publish_changes")
			case "skills-off":
				inputs.RepositorySkills = false
			case "no-local-tools":
				inputs.Tools = []string{}
				inputs.RepositorySkills = false
			case "skill-escape":
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_SKILL_ESCAPE=1")
			case "retained":
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_RETAINED_FIXTURE=1")
			case "mcp-redirect":
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_MCP_BOUNDARY_FIXTURE=1")
			case "failure-boundary":
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_FAILURE_BOUNDARY_FIXTURE=1")
			case "budget-1", "budget-3":
				limit, _ := stepbudget.Parse(tc.corrupt[len("budget-"):])
				inputs.StepBudget = &limit
			case "budget-boundary":
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_BUDGET_BOUNDARY_FIXTURE=1")
			case "session-boundary":
				spec.Environment = append(spec.Environment, "OMNIGREX_PI_SESSION_BOUNDARY_FIXTURE=1")
			}
			stderr := &diagnosticOutput{}
			process, owner, err := composition.StartProtocolDiagnostic(ctx, spec, composition.ProbeOptions{}, stderr, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				if err := process.Stop(cleanup, 5*time.Second); err != nil {
					t.Error(err)
					return
				}
				if err := process.Remove(cleanup); err != nil {
					t.Error(err)
				}
				if err := owner.Close(); err != nil {
					t.Error(err)
				}
			})
			c, err := pi.Start(ctx, process.Transport(), inputs, pi.Options{ExpectedSessionID: "omnigrex-pi-configuration", RequestTimeout: 20 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if tc.corrupt != "" && tc.corrupt != "skills-off" && tc.corrupt != "no-local-tools" && tc.corrupt != "retained" && tc.corrupt != "mcp-redirect" && tc.corrupt != "failure-boundary" && tc.corrupt != "session-boundary" && tc.corrupt != "budget-boundary" && tc.corrupt != "budget-1" && tc.corrupt != "budget-3" {
				if _, err := c.Prepare(ctx); !errors.Is(err, pi.ErrConfiguration) {
					t.Fatalf("invalid preparation did not report a configuration failure: %v", err)
				}
				if _, err := c.Prompt(ctx, "must-not-admit", nil); err == nil {
					t.Fatal("prompt after invalid preparation accepted")
				}
				return
			}
			if tc.prompt == "" {
				if err := pi.CheckConfigurationDiagnostic(ctx, c); err != nil {
					t.Fatalf("configuration: %v; safe stderr: %s", err, stderr.String())
				}
			} else {
				if _, err := c.Prepare(ctx); err != nil {
					t.Fatalf("prepare: %v; stderr: %s", err, stderr.String())
				}
				got, err := c.Prompt(ctx, tc.prompt, nil)
				if tc.prompt == "configuration-provider-error" || tc.prompt == "configuration-provider-stream-error" {
					if err != nil || !got.Admitted || got.Status != agent.Failed || got.FailureClass != "provider_failure" {
						t.Fatalf("provider failure classification: %+v %v", got, err)
					}
					got, err = c.Prompt(ctx, "configuration-provider-error-audit", nil)
				}
				if inputs.StepBudget != nil && (got.StepBudget == nil || !got.StepBudget.Stopped || !got.StepBudget.Complete || got.StepBudget.Used != uint64(inputs.StepBudget.Steps())) {
					t.Fatalf("budget outcome = %+v; evidence=%+v error=%v; stderr: %s", got, got.StepBudget, err, stderr.String())
				}
				if err != nil || got.Status != agent.Completed {
					t.Fatalf("prompt: %+v %v; stderr: %s", got, err, stderr.String())
				}
				if inputs.StepBudget != nil {
					next, err := c.Prompt(ctx, "configuration-budget-audit", nil)
					if err != nil || next.Status != agent.Completed || next.StepBudget == nil || next.StepBudget.Used != 1 || !next.StepBudget.Complete || next.StepBudget.Summary != (inputs.StepBudget.Steps() == 1) {
						t.Fatalf("continued budget/audit: %+v %v; stderr: %s", next, err, stderr.String())
					}
				}
			}
		})
	}
}
