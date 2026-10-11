//go:build integration

package deployment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestPiOperatorExamplesPreserveComposeSelection(t *testing.T) {
	root := repositoryRoot(t)
	guide, err := os.ReadFile(filepath.Join(root, "docs/operator-guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"docker", "microsandbox"} {
		t.Run(backend, func(t *testing.T) {
			directory := t.TempDir()
			files := []string{"compose.pi.yaml", "compose.pi-" + backend + ".yaml"}
			for _, name := range files {
				content, err := os.ReadFile(filepath.Join(root, "deploy", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			overlay := "services:\n  orchestrator:\n    environment:\n      OMNIGREX_DOC_OVERLAY: selected\nvolumes:\n  runtime-state:\n    name: doc-selected-runtime-state\n"
			if err := os.WriteFile(filepath.Join(directory, "compose.instructions.yaml"), []byte(overlay), 0600); err != nil {
				t.Fatal(err)
			}
			files = append(files, "compose.instructions.yaml")
			environment := deploySmokeEnvironment("/dev/null", map[string]string{
				"OMNIGREX_PI_RPC_V1_IMAGE":      testDeployOpencodeImage,
				"OMNIGREX_PI_RPC_V1_PLATFORM":   "linux/amd64",
				"OMNIGREX_MICROSANDBOX_KVM_GID": "992",
			})
			var selected []string
			for _, entry := range environment {
				if !strings.HasPrefix(entry, "COMPOSE_FILE=") {
					selected = append(selected, entry)
				}
			}
			selected = append(selected, "COMPOSE_FILE="+strings.Join(files, ":"))
			section := "### Docker bundle\n"
			if backend == "microsandbox" {
				section = "### Microsandbox bundle\n"
			}
			for _, anchor := range []string{section, "Files are loaded once at orchestrator startup.\n"} {
				_, text, found := strings.Cut(string(guide), anchor)
				if !found {
					t.Fatalf("missing operator example %q", anchor)
				}
				_, text, found = strings.Cut(text, "```sh\n")
				if !found {
					t.Fatal("missing Compose shell example")
				}
				text, _, _ = strings.Cut(text, "\n```")
				for _, line := range strings.Split(text, "\n") {
					words := strings.Fields(line)
					if len(words) < 3 || words[0] != "docker" || words[1] != "compose" {
						continue
					}
					// Render with the exact documented global options instead of
					// performing deployment, exec or image-provisioning mutations.
					var arguments []string
					for _, word := range words[2:] {
						if word == "up" || word == "run" || word == "exec" {
							break
						}
						arguments = append(arguments, word)
					}
					output, err := executeCompose(directory, selected, 30*time.Second, append(arguments, "config", "--format", "json")...)
					if err != nil {
						t.Fatalf("render documented command %q: %v\n%s", line, err, output)
					}
					var document struct {
						Services map[string]struct{ Environment map[string]string }
						Volumes  map[string]struct{ Name string }
					}
					if err := json.Unmarshal(output, &document); err != nil {
						t.Fatal(err)
					}
					if document.Services["orchestrator"].Environment["OMNIGREX_DOC_OVERLAY"] != "selected" || document.Volumes["runtime-state"].Name != "doc-selected-runtime-state" {
						t.Fatalf("documented command %q discarded the selected overrides", line)
					}
				}
			}
		})
	}
}

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
