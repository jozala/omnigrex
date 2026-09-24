package deployment

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func deployTestRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate deployment test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func loadRawCompose(t *testing.T, path string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document
}

func rawServices(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	services, ok := document["services"].(map[string]any)
	if !ok {
		t.Fatalf("services is %T, want map", document["services"])
	}
	return services
}

func rawService(t *testing.T, services map[string]any, name string) map[string]any {
	t.Helper()
	service, ok := services[name].(map[string]any)
	if !ok {
		t.Fatalf("service %q is %T, want map", name, services[name])
	}
	return service
}

func TestDeployComposeMatchesDevelopmentTopology(t *testing.T) {
	root := deployTestRepoRoot(t)
	devPath := filepath.Join(root, "compose.yaml")
	deployPath := filepath.Join(root, "deploy", "compose.yaml")

	devContents, err := os.ReadFile(devPath)
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	deployContents, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatalf("read deploy/compose.yaml: %v", err)
	}
	deployText := string(deployContents)

	dev := loadRawCompose(t, devPath)
	deploy := loadRawCompose(t, deployPath)

	assertExactServiceKeys(t, rawServices(t, dev), "postgres", "opencode-image", "orchestrator")
	assertExactServiceKeys(t, rawServices(t, deploy), "postgres", "opencode-image", "orchestrator")

	devServices := rawServices(t, dev)
	deployServices := rawServices(t, deploy)

	for _, name := range []string{"postgres", "opencode-image", "orchestrator"} {
		service := rawService(t, deployServices, name)
		if _, hasBuild := service["build"]; hasBuild {
			t.Errorf("deploy service %q must not define build", name)
		}
	}
	if strings.Contains(deployText, "build:") {
		t.Errorf("deploy/compose.yaml must not contain build definitions")
	}
	if strings.Contains(deployText, "agent/opencode/Dockerfile") {
		t.Errorf("deploy/compose.yaml must not reference files needed as Docker build context")
	}
	if strings.Contains(deployText, "${OMNIGREX_AGENT_IMAGE_REFERENCE") {
		t.Errorf("deploy/compose.yaml must use a single OpenCode digest and must not interpolate OMNIGREX_AGENT_IMAGE_REFERENCE")
	}

	postgresDev := rawService(t, devServices, "postgres")
	postgresDeploy := rawService(t, deployServices, "postgres")
	if !reflect.DeepEqual(postgresDev, postgresDeploy) {
		t.Errorf("deploy postgres service differs from development postgres service")
	}
	if got := postgresDeploy["image"]; got != "postgres:18-alpine@sha256:b40d931bd0e7ce6eecc59a5a6ac3b3c04a01e559750e73e7086b6dbd7f8bf545" {
		t.Errorf("deploy postgres image = %v, want pinned postgres digest", got)
	}

	validatorDeploy := rawService(t, deployServices, "opencode-image")
	validatorImage, _ := validatorDeploy["image"].(string)
	if !strings.Contains(validatorImage, "${OMNIGREX_OPENCODE_ACP_V1_IMAGE:?") {
		t.Errorf("deploy opencode-image image = %q, want required OMNIGREX_OPENCODE_ACP_V1_IMAGE digest reference", validatorImage)
	}
	if strings.Contains(validatorImage, ":-") {
		t.Errorf("deploy opencode-image image = %q, want required reference without a default", validatorImage)
	}
	if strings.Contains(validatorImage, "omnigrex/opencode:1.18.29") {
		t.Errorf("deploy opencode-image image = %q, want exact digest reference, not a mutable tag", validatorImage)
	}

	orchestratorDev := rawService(t, devServices, "orchestrator")
	orchestratorDeploy := rawService(t, deployServices, "orchestrator")
	orchestratorImage, _ := orchestratorDeploy["image"].(string)
	if !strings.Contains(orchestratorImage, "${OMNIGREX_ORCHESTRATOR_IMAGE:?") {
		t.Errorf("deploy orchestrator image = %q, want required OMNIGREX_ORCHESTRATOR_IMAGE digest reference", orchestratorImage)
	}
	if strings.Contains(orchestratorImage, ":-") {
		t.Errorf("deploy orchestrator image = %q, want required reference without a default", orchestratorImage)
	}
	if strings.Contains(orchestratorImage, "omnigrex/orchestrator:dev") {
		t.Errorf("deploy orchestrator image = %q, want exact digest reference, not a local development tag", orchestratorImage)
	}

	devEnv, _ := orchestratorDev["environment"].(map[string]any)
	deployEnv, ok := orchestratorDeploy["environment"].(map[string]any)
	if !ok {
		t.Fatalf("deploy orchestrator environment is %T, want map", orchestratorDeploy["environment"])
	}
	agentRef, _ := deployEnv["OMNIGREX_AGENT_IMAGE_REFERENCE"].(string)
	runtimeRef, _ := deployEnv["OMNIGREX_OPENCODE_ACP_V1_IMAGE"].(string)
	if !strings.Contains(agentRef, "${OMNIGREX_OPENCODE_ACP_V1_IMAGE:?") {
		t.Errorf("deploy OMNIGREX_AGENT_IMAGE_REFERENCE = %q, want required OMNIGREX_OPENCODE_ACP_V1_IMAGE digest reference", agentRef)
	}
	if !strings.Contains(runtimeRef, "${OMNIGREX_OPENCODE_ACP_V1_IMAGE:?") {
		t.Errorf("deploy OMNIGREX_OPENCODE_ACP_V1_IMAGE = %q, want required digest reference", runtimeRef)
	}
	if agentRef != runtimeRef {
		t.Errorf("deploy readiness and Runtime Profile images differ: %q vs %q, want one shared digest", agentRef, runtimeRef)
	}
	if strings.Contains(agentRef, ":-") || strings.Contains(runtimeRef, ":-") {
		t.Errorf("deploy OpenCode digest references must be required without defaults: %q vs %q", agentRef, runtimeRef)
	}

	for key, devValue := range devEnv {
		if key == "OMNIGREX_AGENT_IMAGE_REFERENCE" {
			continue
		}
		if got, ok := deployEnv[key]; !ok || !reflect.DeepEqual(got, devValue) {
			t.Errorf("deploy orchestrator environment %s = %v, want %v", key, got, devValue)
		}
	}
	for key := range deployEnv {
		if _, ok := devEnv[key]; !ok {
			t.Errorf("deploy orchestrator environment has unexpected key %q", key)
		}
	}

	validatorDev := rawService(t, devServices, "opencode-image")
	for key, devValue := range validatorDev {
		if key == "image" || key == "build" {
			continue
		}
		if got, ok := validatorDeploy[key]; !ok || !reflect.DeepEqual(got, devValue) {
			t.Errorf("deploy opencode-image %s differs from development", key)
		}
	}
	for key, devValue := range orchestratorDev {
		if key == "image" || key == "build" || key == "environment" {
			continue
		}
		if got, ok := orchestratorDeploy[key]; !ok || !reflect.DeepEqual(got, devValue) {
			t.Errorf("deploy orchestrator %s differs from development", key)
		}
	}

	if !reflect.DeepEqual(dev["networks"], deploy["networks"]) {
		t.Errorf("deploy networks differ from development networks")
	}
	if !reflect.DeepEqual(dev["volumes"], deploy["volumes"]) {
		t.Errorf("deploy volumes differ from development volumes")
	}
	if !reflect.DeepEqual(dev["secrets"], deploy["secrets"]) {
		t.Errorf("deploy secrets differ from development secrets")
	}
	assertStableDeployNames(t, deploy)

	devText := string(devContents)
	for _, stable := range []string{"omnigrex-backend", "omnigrex-agent", "omnigrex-postgres", "omnigrex-workspaces", "omnigrex-runtime-state", "omnigrex-mise", "/var/run/docker.sock", "no-new-privileges:true", "10001:10001", "70:70"} {
		if !strings.Contains(deployText, stable) {
			t.Errorf("deploy/compose.yaml missing stable value %q", stable)
		}
		if !strings.Contains(devText, stable) {
			t.Errorf("development compose.yaml missing expected stable value %q", stable)
		}
	}

	envExample, err := os.ReadFile(filepath.Join(root, "deploy", ".env.example"))
	if err != nil {
		t.Fatalf("read deploy/.env.example: %v", err)
	}
	envText := string(envExample)
	if !strings.Contains(envText, "OMNIGREX_ORCHESTRATOR_IMAGE=") {
		t.Errorf("deploy/.env.example must define OMNIGREX_ORCHESTRATOR_IMAGE")
	}
	if !strings.Contains(envText, "OMNIGREX_OPENCODE_ACP_V1_IMAGE=") {
		t.Errorf("deploy/.env.example must define OMNIGREX_OPENCODE_ACP_V1_IMAGE")
	}
	if !strings.Contains(envText, "OMNIGREX_OPENCODE_ACP_V1_PLATFORM=linux/amd64") {
		t.Errorf("deploy/.env.example must select linux/amd64")
	}
	if strings.Contains(envText, "OMNIGREX_AGENT_IMAGE_REFERENCE=") {
		t.Errorf("deploy/.env.example must not define a separate OMNIGREX_AGENT_IMAGE_REFERENCE so the three references cannot diverge")
	}
	for _, relative := range []string{"./secrets/database-password", "./secrets/github-developer-private-key.pem", "./secrets/github-reviewer-private-key.pem", "./secrets/github-webhook-secret", "./secrets/provider-credentials.json"} {
		if !strings.Contains(envText, relative) {
			t.Errorf("deploy/.env.example must resolve secret-file path %q relative to the deployment directory", relative)
		}
	}
}

func assertExactServiceKeys(t *testing.T, services map[string]any, want ...string) {
	t.Helper()
	if len(services) != len(want) {
		keys := make([]string, 0, len(services))
		for key := range services {
			keys = append(keys, key)
		}
		t.Fatalf("services = %v, want exactly %v", keys, want)
	}
	for _, key := range want {
		if _, ok := services[key]; !ok {
			t.Fatalf("services missing %q, want exactly %v", key, want)
		}
	}
}

func assertStableDeployNames(t *testing.T, document map[string]any) {
	t.Helper()
	networks, _ := document["networks"].(map[string]any)
	backend, _ := networks["backend"].(map[string]any)
	agent, _ := networks["agent"].(map[string]any)
	if backend["name"] != "omnigrex-backend" || backend["internal"] != true {
		t.Errorf("deploy backend network = %v, want stable name omnigrex-backend and internal=true", backend)
	}
	if agent["name"] != "omnigrex-agent" || agent["internal"] == true {
		t.Errorf("deploy agent network = %v, want stable name omnigrex-agent and internal=false", agent)
	}
	volumes, _ := document["volumes"].(map[string]any)
	for key, name := range map[string]string{
		"postgres-data": "omnigrex-postgres",
		"workspaces":    "omnigrex-workspaces",
		"runtime-state": "omnigrex-runtime-state",
		"mise-data":     "omnigrex-mise",
	} {
		volume, _ := volumes[key].(map[string]any)
		if volume["name"] != name {
			t.Errorf("deploy volume %s name = %v, want %q", key, volume["name"], name)
		}
	}
}
