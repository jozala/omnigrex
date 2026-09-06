//go:build integration

package deployment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var composeSecretEnvironment = []string{
	"OMNIGREX_DATABASE_PASSWORD_FILE",
	"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE",
	"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE",
	"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE",
	"OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE",
	"OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE",
}

const testDeploymentImage = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate deployment integration test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func composeEnvironment(secretFile, dockerGID, secretGID, httpPort string, additional map[string]string) []string {
	overrides := map[string]string{
		"COMPOSE_ANSI":                                        "never",
		"COMPOSE_FILE":                                        "compose.yaml",
		"COMPOSE_PROJECT_NAME":                                "omnigrex",
		"OMNIGREX_DOCKER_GID":                                 dockerGID,
		"OMNIGREX_SECRET_GID":                                 secretGID,
		"OMNIGREX_HTTP_PORT":                                  httpPort,
		"OMNIGREX_AGENT_IMAGE_REFERENCE":                      "omnigrex/opencode:1.18.19",
		"OMNIGREX_GITHUB_DEVELOPER_APP_ID":                    "1",
		"OMNIGREX_GITHUB_REVIEWER_APP_ID":                     "2",
		"OMNIGREX_READINESS_TIMEOUT":                          "15s",
		"OMNIGREX_SHUTDOWN_TIMEOUT":                           "10s",
		"OMNIGREX_OPENCODE_ACP_V1_IMAGE":                      testDeploymentImage,
		"OMNIGREX_OPENCODE_ACP_V1_PLATFORM":                   "linux/amd64",
		"OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE": "",
	}
	for _, name := range composeSecretEnvironment {
		overrides[name] = secretFile
	}
	for name, value := range additional {
		overrides[name] = value
	}

	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[name]; !replace {
			environment = append(environment, entry)
		}
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func executeCompose(root string, environment []string, timeout time.Duration, arguments ...string) ([]byte, error) {
	return executeCommand(root, environment, timeout, "docker", append([]string{"compose"}, arguments...)...)
}

func executeDocker(root string, environment []string, timeout time.Duration, arguments ...string) ([]byte, error) {
	return executeCommand(root, environment, timeout, "docker", arguments...)
}

func executeCommand(root string, environment []string, timeout time.Duration, executable string, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = root
	command.Env = environment
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, fmt.Errorf("%s %s: %w", executable, strings.Join(arguments, " "), ctx.Err())
	}
	if err != nil {
		return output, fmt.Errorf("%s %s: %w", executable, strings.Join(arguments, " "), err)
	}
	return output, nil
}
