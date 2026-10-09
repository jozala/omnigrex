package pi

import (
	"context"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// DiagnosticSpec requires the content-addressed disposable Pi protocol fixture image.
// It does not accept provider keys, extensions, skills, or repository configuration.
func DiagnosticSpec(image string, platform execution.Platform) execution.DiagnosticSpec {
	return execution.DiagnosticSpec{
		Namespace: "omnigrex-runtime-state", Image: image, Platform: platform,
		Command:              []string{"/usr/local/bin/omnigrex-pi-diagnostic"},
		Environment:          []string{"HOME=/tmp", "PI_CODING_AGENT_DIR=/run/pi-agent", "PI_OFFLINE=1", "PI_TELEMETRY=0", "PI_SKIP_VERSION_CHECK=1"},
		TemporaryDirectories: []execution.TemporaryDirectory{{Target: "/workspace", SizeBytes: 16 << 20}, {Target: "/run/pi-agent", SizeBytes: 16 << 20}, {Target: "/tmp", SizeBytes: 64 << 20}},
		MemoryBytes:          512 << 20,
	}
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
