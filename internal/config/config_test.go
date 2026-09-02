package config_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestLoadUsesDefaults(t *testing.T) {
	got, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got.DatabaseURL != "postgres://omnigrex@postgres:5432/omnigrex?sslmode=disable" {
		t.Errorf("DatabaseURL = %q, want %q", got.DatabaseURL, "postgres://omnigrex@postgres:5432/omnigrex?sslmode=disable")
	}
	if got.DatabasePasswordSecretFile != "/run/secrets/omnigrex-database-password" {
		t.Errorf("DatabasePasswordSecretFile = %q, want %q", got.DatabasePasswordSecretFile, "/run/secrets/omnigrex-database-password")
	}
	if got.DockerAgentNetwork != "omnigrex-agent" {
		t.Errorf("DockerAgentNetwork = %q, want %q", got.DockerAgentNetwork, "omnigrex-agent")
	}
	if got.WorkspaceVolume != "omnigrex-workspaces" {
		t.Errorf("WorkspaceVolume = %q, want %q", got.WorkspaceVolume, "omnigrex-workspaces")
	}
	if got.RuntimeStateVolume != "omnigrex-runtime-state" {
		t.Errorf("RuntimeStateVolume = %q, want %q", got.RuntimeStateVolume, "omnigrex-runtime-state")
	}
	if got.MiseVolume != "omnigrex-mise" {
		t.Errorf("MiseVolume = %q, want %q", got.MiseVolume, "omnigrex-mise")
	}
	if got.AgentImageReference != "omnigrex/opencode:1.18.19" {
		t.Errorf("AgentImageReference = %q, want %q", got.AgentImageReference, "omnigrex/opencode:1.18.19")
	}
	if got.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", got.HTTPAddr, ":8080")
	}
	if got.ReadinessTimeout != 15*time.Second {
		t.Errorf("ReadinessTimeout = %s, want %s", got.ReadinessTimeout, 15*time.Second)
	}
	if got.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %s, want %s", got.ShutdownTimeout, 10*time.Second)
	}
}

func TestLoadUsesEnvironment(t *testing.T) {
	values := map[string]string{
		"OMNIGREX_DATABASE_URL":                  "postgres://app@database/app",
		"OMNIGREX_DATABASE_PASSWORD_SECRET_FILE": "/secrets/database-password",
		"OMNIGREX_DOCKER_AGENT_NETWORK":          "agents",
		"OMNIGREX_WORKSPACE_VOLUME":              "workspaces",
		"OMNIGREX_RUNTIME_STATE_VOLUME":          "runtime-state",
		"OMNIGREX_MISE_VOLUME":                   "mise",
		"OMNIGREX_AGENT_IMAGE_REFERENCE":         "registry.example/agent:v2",
		"OMNIGREX_HTTP_ADDR":                     "127.0.0.1:9000",
		"OMNIGREX_READINESS_TIMEOUT":             "3s",
		"OMNIGREX_SHUTDOWN_TIMEOUT":              "20s",
	}

	got, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got.DatabaseURL != values["OMNIGREX_DATABASE_URL"] {
		t.Errorf("DatabaseURL = %q, want %q", got.DatabaseURL, values["OMNIGREX_DATABASE_URL"])
	}
	if got.DatabasePasswordSecretFile != values["OMNIGREX_DATABASE_PASSWORD_SECRET_FILE"] {
		t.Errorf("DatabasePasswordSecretFile = %q, want %q", got.DatabasePasswordSecretFile, values["OMNIGREX_DATABASE_PASSWORD_SECRET_FILE"])
	}
	if got.DockerAgentNetwork != values["OMNIGREX_DOCKER_AGENT_NETWORK"] {
		t.Errorf("DockerAgentNetwork = %q, want %q", got.DockerAgentNetwork, values["OMNIGREX_DOCKER_AGENT_NETWORK"])
	}
	if got.WorkspaceVolume != values["OMNIGREX_WORKSPACE_VOLUME"] {
		t.Errorf("WorkspaceVolume = %q, want %q", got.WorkspaceVolume, values["OMNIGREX_WORKSPACE_VOLUME"])
	}
	if got.RuntimeStateVolume != values["OMNIGREX_RUNTIME_STATE_VOLUME"] {
		t.Errorf("RuntimeStateVolume = %q, want %q", got.RuntimeStateVolume, values["OMNIGREX_RUNTIME_STATE_VOLUME"])
	}
	if got.MiseVolume != values["OMNIGREX_MISE_VOLUME"] {
		t.Errorf("MiseVolume = %q, want %q", got.MiseVolume, values["OMNIGREX_MISE_VOLUME"])
	}
	if got.AgentImageReference != values["OMNIGREX_AGENT_IMAGE_REFERENCE"] {
		t.Errorf("AgentImageReference = %q, want %q", got.AgentImageReference, values["OMNIGREX_AGENT_IMAGE_REFERENCE"])
	}
	if got.HTTPAddr != values["OMNIGREX_HTTP_ADDR"] {
		t.Errorf("HTTPAddr = %q, want %q", got.HTTPAddr, values["OMNIGREX_HTTP_ADDR"])
	}
	if got.ReadinessTimeout != 3*time.Second {
		t.Errorf("ReadinessTimeout = %s, want %s", got.ReadinessTimeout, 3*time.Second)
	}
	if got.ShutdownTimeout != 20*time.Second {
		t.Errorf("ShutdownTimeout = %s, want %s", got.ShutdownTimeout, 20*time.Second)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "database URL", key: "OMNIGREX_DATABASE_URL", value: "not-a-database-url"},
		{name: "database password secret file", key: "OMNIGREX_DATABASE_PASSWORD_SECRET_FILE", value: "relative/password"},
		{name: "Docker agent network", key: "OMNIGREX_DOCKER_AGENT_NETWORK", value: "   "},
		{name: "workspace volume", key: "OMNIGREX_WORKSPACE_VOLUME", value: "   "},
		{name: "runtime-state volume", key: "OMNIGREX_RUNTIME_STATE_VOLUME", value: "   "},
		{name: "mise volume", key: "OMNIGREX_MISE_VOLUME", value: "   "},
		{name: "agent image reference", key: "OMNIGREX_AGENT_IMAGE_REFERENCE", value: "   "},
		{name: "readiness timeout syntax", key: "OMNIGREX_READINESS_TIMEOUT", value: "eventually"},
		{name: "readiness timeout value", key: "OMNIGREX_READINESS_TIMEOUT", value: "0s"},
		{name: "shutdown timeout syntax", key: "OMNIGREX_SHUTDOWN_TIMEOUT", value: "eventually"},
		{name: "shutdown timeout value", key: "OMNIGREX_SHUTDOWN_TIMEOUT", value: "-1s"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Load(func(key string) string {
				if key == test.key {
					return test.value
				}
				return ""
			})
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
		})
	}
}
