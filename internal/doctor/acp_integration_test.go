//go:build integration

package doctor

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestACPProbeInitializesOpenCodeWithoutPersistentState(t *testing.T) {
	output, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", "omnigrex/opencode:1.18.29").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect OpenCode image: %v\n%s", err, output)
	}
	imageID := strings.TrimSpace(string(output))
	profile, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@"+imageID,
		runtimeprofile.Platform{OS: "linux", Arch: runtime.GOARCH},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := checkACPImage(ctx, profile, imageID, acpProbeOptions{
		Network: "bridge", MCPHost: "host.docker.internal", ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}); err != nil {
		t.Fatalf("ACP probe error = %v", err)
	}
}
