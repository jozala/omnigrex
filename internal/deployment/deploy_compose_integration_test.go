//go:build integration

package deployment

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

func TestDeployImageOnlyStartupSmoke(t *testing.T) {
	if os.Getenv("OMNIGREX_DEPLOY_SMOKE") != "1" {
		t.Skip("set OMNIGREX_DEPLOY_SMOKE=1 with published digest fixtures to start the image-only stack")
	}
	orchestratorImage := os.Getenv("OMNIGREX_DEPLOY_SMOKE_ORCHESTRATOR_IMAGE")
	opencodeImage := os.Getenv("OMNIGREX_DEPLOY_SMOKE_OPENCODE_IMAGE")
	if orchestratorImage == "" || opencodeImage == "" {
		t.Skip("set OMNIGREX_DEPLOY_SMOKE_ORCHESTRATOR_IMAGE and OMNIGREX_DEPLOY_SMOKE_OPENCODE_IMAGE to exact digests")
	}
	if !strings.Contains(orchestratorImage, "@sha256:") || !strings.Contains(opencodeImage, "@sha256:") {
		t.Fatalf("smoke images must be exact digests, got %q and %q", orchestratorImage, opencodeImage)
	}

	secretDirectory := t.TempDir()
	databaseSecret := writeTestSecret(t, secretDirectory, "database-password", []byte("deploy-smoke-secret\n"))
	webhookSecret := writeTestSecret(t, secretDirectory, "webhook-secret", []byte("deploy-smoke-webhook\n"))
	providerCredentials := writeTestSecret(t, secretDirectory, "provider-credentials.json", []byte(`{"provider":"deploy-smoke"}`))
	developerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate Developer test key: %v", err)
	}
	reviewerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate Reviewer test key: %v", err)
	}
	developerKeyFile := writeTestSecret(t, secretDirectory, "github-developer-app.pem", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(developerKey),
	}))
	reviewerKeyFile := writeTestSecret(t, secretDirectory, "github-reviewer-app.pem", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(reviewerKey),
	}))

	dockerGID := os.Getenv("OMNIGREX_DOCKER_GID")
	if dockerGID == "" {
		dockerGID = "0"
	}
	httpPort := freeHTTPPort(t)
	const projectName = "omnigrex-deploy-smoke"
	agentNetwork := projectName + "-agent"
	backendNetwork := projectName + "-backend"
	postgresVolume := projectName + "-postgres"
	workspacesVolume := projectName + "-workspaces"
	runtimeVolume := projectName + "-runtime-state"
	miseVolume := projectName + "-mise"

	overridePath := filepath.Join(t.TempDir(), "deploy-smoke-override.yaml")
	overrideYAML := "networks:\n" +
		"  backend:\n    name: " + backendNetwork + "\n" +
		"  agent:\n    name: " + agentNetwork + "\n" +
		"volumes:\n" +
		"  postgres-data:\n    name: " + postgresVolume + "\n" +
		"  workspaces:\n    name: " + workspacesVolume + "\n" +
		"  runtime-state:\n    name: " + runtimeVolume + "\n" +
		"  mise-data:\n    name: " + miseVolume + "\n" +
		"services:\n" +
		"  orchestrator:\n    environment:\n" +
		"      OMNIGREX_DOCKER_AGENT_NETWORK: " + agentNetwork + "\n" +
		"      OMNIGREX_WORKSPACE_VOLUME: " + workspacesVolume + "\n" +
		"      OMNIGREX_RUNTIME_STATE_VOLUME: " + runtimeVolume + "\n" +
		"      OMNIGREX_MISE_VOLUME: " + miseVolume + "\n"
	if err := os.WriteFile(overridePath, []byte(overrideYAML), 0o600); err != nil {
		t.Fatalf("write disposable override file: %v", err)
	}

	base := composeEnvironment(databaseSecret, dockerGID, strconv.Itoa(os.Getgid()), httpPort, map[string]string{
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE": developerKeyFile,
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":  reviewerKeyFile,
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":        webhookSecret,
		"OMNIGREX_PROVIDER_CREDENTIALS_FILE":         providerCredentials,
		"OMNIGREX_ORCHESTRATOR_IMAGE":                orchestratorImage,
		"OMNIGREX_OPENCODE_ACP_V1_IMAGE":             opencodeImage,
	})
	environment := make([]string, 0, len(base)+1)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if name == "COMPOSE_FILE" || name == "COMPOSE_PROJECT_NAME" {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "COMPOSE_PROJECT_NAME="+projectName)

	root := repositoryRoot(t)
	composeFiles := []string{"-f", "deploy/compose.yaml", "-f", overridePath}
	runCompose := func(timeout time.Duration, args ...string) string {
		t.Helper()
		full := append(append([]string{}, composeFiles...), args...)
		output, err := executeCompose(root, environment, timeout, full...)
		if err != nil {
			t.Fatalf("%v\n%s", err, output)
		}
		return strings.TrimSpace(string(output))
	}

	t.Cleanup(func() {
		output, err := executeCompose(root, environment, 2*time.Minute, append(composeFiles, "down", "--volumes", "--remove-orphans", "--timeout", "10")...)
		if err != nil {
			t.Logf("clean disposable deploy smoke resources: %v\n%s", err, output)
		}
	})

	runCompose(2*time.Minute, "down", "--volumes", "--remove-orphans", "--timeout", "10")
	// Start the image-only stack without building images.
	runCompose(10*time.Minute, "up", "--wait")

	baseURL := "http://127.0.0.1:" + httpPort
	waitForHTTPStatus(t, baseURL+"/healthz", http.StatusOK, 90*time.Second)
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 90*time.Second)
}
