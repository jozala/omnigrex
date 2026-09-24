//go:build integration

package deployment

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	testDeployOrchestratorImage = "registry.example/omnigrex/orchestrator@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDeployOpencodeImage     = "registry.example/omnigrex/opencode@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

func deploySmokeEnvironment(secretFile string, additional map[string]string) []string {
	overrides := map[string]string{
		"OMNIGREX_ORCHESTRATOR_IMAGE":    testDeployOrchestratorImage,
		"OMNIGREX_AGENT_TURN_MEMORY_MIB": "512",
	}
	for name, value := range additional {
		overrides[name] = value
	}
	base := composeEnvironment(secretFile, "123", "456", "8080", overrides)
	environment := make([]string, 0, len(base))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if name == "COMPOSE_FILE" || name == "COMPOSE_PROJECT_NAME" || name == "OMNIGREX_OPENCODE_ACP_V1_IMAGE" {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"COMPOSE_FILE=deploy/compose.yaml",
		"COMPOSE_PROJECT_NAME=omnigrex-deploy-smoke",
		"OMNIGREX_OPENCODE_ACP_V1_IMAGE="+testDeployOpencodeImage,
	)
	return environment
}

func TestDeployComposeSmoke(t *testing.T) {
	environment := deploySmokeEnvironment("/dev/null", nil)
	output, err := executeCompose(repositoryRoot(t), environment, 30*time.Second, "config", "--format", "json")
	if err != nil {
		t.Fatalf("render deploy/compose.yaml: %v\n%s", err, output)
	}
	var config composeConfig
	if err := json.Unmarshal(output, &config); err != nil {
		t.Fatalf("decode deploy Compose JSON: %v\n%s", err, output)
	}

	assertExactKeys(t, "services", config.Services, "postgres", "opencode-image", "orchestrator")
	postgres := config.Services["postgres"]
	validator := config.Services["opencode-image"]
	orchestrator := config.Services["orchestrator"]

	if postgres.Image != postgresImage {
		t.Errorf("deploy postgres image = %q, want pinned %q", postgres.Image, postgresImage)
	}
	if validator.Image != testDeployOpencodeImage {
		t.Errorf("deploy opencode-image image = %q, want single exact digest %q", validator.Image, testDeployOpencodeImage)
	}
	if orchestrator.Image != testDeployOrchestratorImage {
		t.Errorf("deploy orchestrator image = %q, want exact digest %q", orchestrator.Image, testDeployOrchestratorImage)
	}
	if !strings.Contains(validator.Image, "@sha256:") || !strings.Contains(orchestrator.Image, "@sha256:") {
		t.Errorf("deploy images must be exact digests: %q vs %q", validator.Image, orchestrator.Image)
	}
	if got := orchestrator.Environment["OMNIGREX_AGENT_IMAGE_REFERENCE"]; got != testDeployOpencodeImage {
		t.Errorf("deploy readiness image reference = %q, want single digest %q", got, testDeployOpencodeImage)
	}
	if got := orchestrator.Environment["OMNIGREX_OPENCODE_ACP_V1_IMAGE"]; got != testDeployOpencodeImage {
		t.Errorf("deploy Runtime Profile image = %q, want single digest %q", got, testDeployOpencodeImage)
	}
	if validator.Image != orchestrator.Environment["OMNIGREX_AGENT_IMAGE_REFERENCE"] ||
		validator.Image != orchestrator.Environment["OMNIGREX_OPENCODE_ACP_V1_IMAGE"] {
		t.Errorf("deploy opencode validation, readiness, and Runtime Profile images must match: %q vs %q vs %q",
			validator.Image, orchestrator.Environment["OMNIGREX_AGENT_IMAGE_REFERENCE"], orchestrator.Environment["OMNIGREX_OPENCODE_ACP_V1_IMAGE"])
	}

	assertServiceSandbox(t, "postgres", postgres, "70:70")
	assertServiceSandbox(t, "opencode-image", validator, "10001:10001")
	assertServiceSandbox(t, "orchestrator", orchestrator, "10001:10001")
	assertSoleDockerSocketMount(t, config.Services, "orchestrator")
	assertStableNetworks(t, config.Networks)
	assertStableVolumes(t, config.Volumes)

	devEnvironment := composeEnvironment("/dev/null", "123", "456", "8080", nil)
	devOutput, err := executeCompose(repositoryRoot(t), devEnvironment, 30*time.Second, "config", "--format", "json")
	if err != nil {
		t.Fatalf("render compose.yaml: %v\n%s", err, devOutput)
	}
	var devConfig composeConfig
	if err := json.Unmarshal(devOutput, &devConfig); err != nil {
		t.Fatalf("decode development Compose JSON: %v\n%s", err, devOutput)
	}
	if devConfig.Services["postgres"].Image != config.Services["postgres"].Image {
		t.Errorf("deploy postgres image %q differs from development %q", config.Services["postgres"].Image, devConfig.Services["postgres"].Image)
	}
	if len(devConfig.Networks) != len(config.Networks) || len(devConfig.Volumes) != len(config.Volumes) {
		t.Errorf("deploy networks/volumes differ from development stack")
	}

	if os.Getenv("OMNIGREX_DEPLOY_SMOKE_SKIP_IMAGE_INSPECT") != "" {
		t.Skip("skipping disposable image inspect")
	}
	if _, err := executeDocker(repositoryRoot(t), environment, 30*time.Second, "image", "inspect", readinessImage); err != nil {
		t.Logf("disposable readiness image inspect skipped: %v", err)
	}
}
