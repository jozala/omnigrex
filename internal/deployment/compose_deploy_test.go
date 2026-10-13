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

func TestPiDeploymentArtifacts(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "../..")
	load := func(name string) map[string]any {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(content, &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	dev := load("compose.yaml")
	deploy := load("deploy/compose.pi.yaml")
	for _, key := range []string{"networks", "volumes"} {
		if !reflect.DeepEqual(dev[key], deploy[key]) {
			t.Errorf("%s differ across developer and published topology", key)
		}
	}
	for _, name := range []string{"compose.yaml", "deploy/compose.pi.yaml"} {
		document := load(name)
		services := document["services"].(map[string]any)
		if len(services) != 3 {
			t.Fatalf("%s has unexpected services: %v", name, services)
		}
		for _, serviceName := range []string{"postgres", "pi-image", "orchestrator"} {
			service, ok := services[serviceName].(map[string]any)
			if !ok {
				t.Fatalf("%s lacks %s", name, serviceName)
			}
			if name != "compose.yaml" && service["build"] != nil {
				t.Errorf("published %s has build context", serviceName)
			}
			image := service["image"].(string)
			if serviceName == "postgres" && !strings.Contains(image, "@sha256:") {
				t.Error("PostgreSQL is not pinned")
			}
			if serviceName == "pi-image" && !strings.Contains(image, "${OMNIGREX_PI_RPC_V1_IMAGE:?") {
				t.Error("Pi validator does not require exact configured image")
			}
			if name != "compose.yaml" && serviceName == "orchestrator" && !strings.Contains(image, "${OMNIGREX_ORCHESTRATOR_IMAGE:?") {
				t.Error("published control plane has image fallback")
			}
			if service["read_only"] != true || service["user"] == nil {
				t.Errorf("%s lacks non-root read-only configuration", serviceName)
			}
			if environment, ok := service["environment"].(map[string]any); ok {
				for key := range environment {
					if strings.Contains(key, "OPENCODE") || key == "OMNIGREX_AGENT_IMAGE_REFERENCE" {
						t.Errorf("obsolete configuration %s", key)
					}
				}
			}
		}
	}
	for _, name := range []string{".env.example", "deploy/.env.pi.example"} {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		for _, required := range []string{"OMNIGREX_PI_RPC_V1_IMAGE=", "OMNIGREX_PI_RPC_V1_PLATFORM=linux/amd64", "./secrets/database-password", "./secrets/github-developer-private-key.pem", "./secrets/github-reviewer-private-key.pem", "./secrets/github-webhook-secret", "./secrets/provider-credentials.json"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s missing %s", name, required)
			}
		}
		if strings.Contains(text, "OPENCODE_ACP") || strings.Contains(text, "OMNIGREX_AGENT_IMAGE_REFERENCE=") {
			t.Errorf("%s contains obsolete settings", name)
		}
	}
}
