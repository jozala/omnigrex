package developmentenvironment_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/repoenvironment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func TestGeneratedEnvironmentIsAcceptedByRepositoryLoader(t *testing.T) {
	image := "ghcr.io/jozala/omnigrex/development@sha256:" + strings.Repeat("a", 64)
	output, err := exec.Command("sh", "../write-development-environment.sh", image).CombinedOutput()
	if err != nil {
		t.Fatalf("generate environment: %v\n%s", err, output)
	}
	definition, err := repoenvironment.Parse(output)
	if err != nil {
		t.Fatalf("generated environment was rejected: %v\n%s", err, output)
	}
	want := []execution.Requirement{execution.HardwareIsolation, execution.NestedContainers, execution.ToolInstallation}
	if definition.Image != image || definition.Platform != "linux/amd64" || !slices.Equal(definition.Requirements, want) {
		t.Fatalf("unexpected repository environment: %+v", definition)
	}
}

func TestEnvironmentGenerationRejectsUnpinnedOrMalformedImages(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/jozala/omnigrex/development:latest",
		"ghcr.io/jozala/omnigrex/development:tag@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/jozala/omnigrex/development@sha256:" + strings.Repeat("a", 63),
		"ghcr.io/jozala/omnigrex/development@sha256:" + strings.Repeat("A", 64),
		"ghcr.io/jozala/omnigrex/development@sha256:" + strings.Repeat("a", 64) + "\nplatform: linux/arm64",
	} {
		t.Run(image, func(t *testing.T) {
			output, err := exec.Command("sh", "../write-development-environment.sh", image).CombinedOutput()
			if err == nil || len(output) != 0 {
				t.Fatalf("invalid image generated configuration: %v\n%s", err, output)
			}
		})
	}
}
