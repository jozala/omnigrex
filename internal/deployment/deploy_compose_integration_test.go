//go:build integration

package deployment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	testDeployOrchestratorImage = "registry.example/omnigrex/orchestrator@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDeployPiImage           = "registry.example/omnigrex/pi@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

func deploySmokeEnvironment(secretFile string, additional map[string]string) []string {
	overrides := map[string]string{
		"OMNIGREX_ORCHESTRATOR_IMAGE":    testDeployOrchestratorImage,
		"OMNIGREX_PI_RPC_V1_IMAGE":       testDeployPiImage,
		"OMNIGREX_AGENT_TURN_MEMORY_MIB": "1024",
		"COMPOSE_FILE":                   "deploy/compose.pi.yaml:deploy/compose.pi-docker.yaml",
		"COMPOSE_PROJECT_NAME":           "omnigrex-deploy-smoke",
	}
	for name, value := range additional {
		overrides[name] = value
	}
	return composeEnvironment(secretFile, "123", "456", "8080", overrides)
}

func TestDeployComposeSmoke(t *testing.T) {
	output, err := executeCompose(repositoryRoot(t), deploySmokeEnvironment("/dev/null", nil), 30*time.Second, "config", "--format", "json")
	if err != nil {
		t.Fatalf("render Pi Docker bundle: %v\n%s", err, output)
	}
	var config composeConfig
	if err := json.Unmarshal(output, &config); err != nil {
		t.Fatal(err)
	}
	assertExactKeys(t, "services", config.Services, "postgres", "pi-image", "orchestrator")
	if config.Services["postgres"].Image != postgresImage || config.Services["orchestrator"].Image != testDeployOrchestratorImage {
		t.Fatal("deployment must use the pinned PostgreSQL and selected orchestrator images")
	}
	validator := config.Services["pi-image"]
	orchestrator := config.Services["orchestrator"]
	if validator.Image != testDeployPiImage || validator.Image != orchestrator.Environment["OMNIGREX_PI_RPC_V1_IMAGE"] {
		t.Fatal("validator and Runtime Profile must select the same exact Pi digest")
	}
	for name, service := range config.Services {
		user := "10001:10001"
		if name == "postgres" {
			user = "70:70"
		}
		assertServiceSandbox(t, name, service, user)
		for key := range service.Environment {
			if strings.Contains(key, "OPENCODE") || key == "OMNIGREX_AGENT_IMAGE_REFERENCE" {
				t.Errorf("obsolete runtime configuration %s", key)
			}
		}
	}
	assertSoleDockerSocketMount(t, config.Services, "orchestrator")
	assertStableNetworks(t, config.Networks)
	assertStableVolumes(t, config.Volumes)
}
