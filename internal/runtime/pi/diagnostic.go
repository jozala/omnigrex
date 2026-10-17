package pi

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/workflow"
)

// DiagnosticSpec requires the content-addressed disposable Pi protocol fixture image.
// It does not accept provider keys, extensions, skills, or repository configuration.
func DiagnosticSpec(image string, platform execution.Platform) execution.DiagnosticSpec {
	return execution.DiagnosticSpec{
		Namespace: "omnigrex-runtime-state", Image: image, Platform: platform,
		Command:              []string{"/usr/local/bin/omnigrex-pi-diagnostic"},
		Environment:          []string{"HOME=/tmp", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "PI_CODING_AGENT_DIR=/run/pi-agent", "PI_OFFLINE=1", "PI_TELEMETRY=0", "PI_SKIP_VERSION_CHECK=1"},
		TemporaryDirectories: []execution.TemporaryDirectory{{Target: "/workspace", SizeBytes: 16 << 20}, {Target: "/run/pi-agent", SizeBytes: 16 << 20}, {Target: "/tmp", SizeBytes: 64 << 20}},
		MemoryBytes:          512 << 20,
	}
}

// ConfigurationDiagnostic supplies fixture-only credentials to the owned
// runner and the real gateway's disposable in-memory authority.
func ConfigurationDiagnostic(roleID role.ID) (Configuration, error) {
	policy, ok := role.BuiltinPolicyCatalog().Lookup(roleID)
	if !ok {
		return Configuration{}, ErrConfiguration
	}
	permissions := "  read: allow\n  skill: allow\n  grep: allow\n  find: allow\n  ls: allow\n  bash: allow\n"
	keyByte := byte(2)
	if roleID == role.Developer {
		permissions += "  edit: allow\n  write: allow\n"
		keyByte = 1
	}
	p, err := agentprofile.Parse(".omnigrex/team/fixture.md", []byte(fmt.Sprintf("---\nname: fixture\nrole: %s\nruntime: pi-rpc/v2\nmodel: diagnostic/controlled\nthinking: off\npermissions:\n%s---\nCURRENT_OPERATOR_GUIDANCE\n", roleID, permissions)), role.BuiltinPolicyCatalog())
	if err != nil {
		return Configuration{}, err
	}
	endpoint := agent.MCPEndpoint{Name: "omnigrex", URL: "http://127.0.0.1:8844/mcp", Headers: []agent.Header{{Name: "Authorization", Value: "Bearer " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{keyByte}, 32))}}}
	return Render(p, policy, agentinstructions.Operator{}, workflow.StageDefinition{Role: roleID, Instructions: "CURRENT_STAGE_OBJECTIVE"}, workflow.TurnPurpose("fixture"), endpoint, []byte(`{"diagnostic":{"type":"api_key","key":"configuration-only-key-$$-$!-$9"},"unrelated":{"type":"api_key","key":"must-not-be-delivered"}}`))
}

func ConfigurationDiagnosticSpec(image string, platform execution.Platform) execution.DiagnosticSpec {
	spec := DiagnosticSpec(image, platform)
	spec.Command = []string{"/usr/local/bin/node", "/opt/omnigrex/pi/configuration.mjs"}
	return spec
}

func CheckConfigurationDiagnostic(ctx context.Context, connection agent.Connection) error {
	if _, err := connection.Prepare(ctx); err != nil {
		return err
	}
	for _, prompt := range []string{"configuration-audit", "configuration-mcp", "configuration-write", "configuration-edit", "configuration-skill", "configuration-grep", "configuration-find", "configuration-ls", "configuration-bash", "configuration-audit"} {
		outcome, err := connection.Prompt(ctx, prompt, nil)
		if err != nil {
			return err
		}
		if outcome.Status != agent.Completed {
			return agent.ErrRuntime
		}
	}
	return nil
}

func CheckDiagnostic(ctx context.Context, connection agent.Connection) error {
	if _, err := connection.Prepare(ctx); err != nil {
		return err
	}
	outcome, err := connection.Prompt(ctx, "protocol-success", nil)
	if err != nil {
		return err
	}
	if outcome.Status != agent.Completed {
		return agent.ErrRuntime
	}
	return nil
}
