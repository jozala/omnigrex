//go:build integration

package deployment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestPiDeployBackendRequirements(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		t.Run(backend, func(t *testing.T) {
			environment := deploySmokeEnvironment("/dev/null", map[string]string{
				"OMNIGREX_PI_RPC_V1_IMAGE":      testDeployOpencodeImage,
				"OMNIGREX_PI_RPC_V1_PLATFORM":   "linux/amd64",
				"OMNIGREX_MICROSANDBOX_KVM_GID": "992",
			})
			var filtered []string
			for _, entry := range environment {
				name, _, _ := strings.Cut(entry, "=")
				if name == "COMPOSE_FILE" || name == "OMNIGREX_OPENCODE_ACP_V1_IMAGE" || name == "OMNIGREX_OPENCODE_ACP_V1_PLATFORM" || backend == "docker" && name == "OMNIGREX_MICROSANDBOX_KVM_GID" || backend == "microsandbox" && name == "OMNIGREX_DOCKER_GID" {
					continue
				}
				filtered = append(filtered, entry)
			}
			args := []string{"-f", "deploy/compose.pi.yaml", "-f", "deploy/compose.pi-" + backend + ".yaml", "config", "--format", "json"}
			output, err := executeCompose(repositoryRoot(t), filtered, 30*time.Second, args...)
			if err != nil {
				t.Fatalf("render %s: %v\n%s", backend, err, output)
			}
			var document struct {
				Services map[string]struct {
					Environment map[string]string
					Build       any
					Volumes     []struct{ Target string }
					Devices     []any
				}
			}
			if err := json.Unmarshal(output, &document); err != nil {
				t.Fatal(err)
			}
			orchestrator := document.Services["orchestrator"]
			if orchestrator.Environment["OMNIGREX_EXECUTION_BACKEND"] != backend || orchestrator.Environment["OMNIGREX_OPENCODE_ACP_V1_IMAGE"] != "" {
				t.Fatal("incorrect operational selection")
			}
			socket := false
			for _, volume := range orchestrator.Volumes {
				if volume.Target == "/var/run/docker.sock" {
					socket = true
				}
			}
			if socket != (backend == "docker") || (len(orchestrator.Devices) > 0) != (backend == "microsandbox") {
				t.Fatal("foreign host authority or missing selected-backend device")
			}
			for name, service := range document.Services {
				if service.Build != nil {
					t.Fatalf("%s requires a source checkout", name)
				}
			}
		})
	}
}

func TestPiDeployHealthcheckBudget(t *testing.T) {
	for _, test := range []struct {
		name, readiness, healthcheck string
	}{
		{"defaults", "", ""},
		{"longer configured budget", "90s", "100s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := deploySmokeEnvironment("/dev/null", map[string]string{
				"OMNIGREX_PI_RPC_V1_IMAGE":     testDeployOpencodeImage,
				"OMNIGREX_PI_RPC_V1_PLATFORM":  "linux/amd64",
				"OMNIGREX_READINESS_TIMEOUT":   test.readiness,
				"OMNIGREX_HEALTHCHECK_TIMEOUT": test.healthcheck,
			})
			var filtered []string
			for _, entry := range environment {
				if !strings.HasPrefix(entry, "COMPOSE_FILE=") {
					filtered = append(filtered, entry)
				}
			}
			output, err := executeCompose(repositoryRoot(t), filtered, 30*time.Second, "-f", "deploy/compose.pi.yaml", "-f", "deploy/compose.pi-docker.yaml", "config", "--format", "json")
			if err != nil {
				t.Fatalf("render healthcheck budgets: %v\n%s", err, output)
			}
			var document struct {
				Services map[string]struct {
					Environment map[string]string
					Healthcheck struct{ Timeout string }
				}
			}
			if err := json.Unmarshal(output, &document); err != nil {
				t.Fatal(err)
			}
			service := document.Services["orchestrator"]
			readiness, err := config.LoadReadinessTimeout(service.Environment["OMNIGREX_READINESS_TIMEOUT"])
			if err != nil {
				t.Fatal(err)
			}
			outer, err := time.ParseDuration(service.Healthcheck.Timeout)
			if err != nil {
				t.Fatal(err)
			}
			if outer <= readiness+config.HealthcheckResponseAllowance {
				t.Fatalf("Compose deadline %s cuts off HTTP readiness budget %s plus response allowance", outer, readiness)
			}
			if test.readiness == "" && readiness != time.Minute {
				t.Fatalf("default readiness budget = %s", readiness)
			}
		})
	}
}
