package config_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestLoadUsesDefaults(t *testing.T) {
	got, err := config.Load(environment(nil))
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
	if got.GitHubAPIURL != "https://api.github.com" {
		t.Errorf("GitHubAPIURL = %q, want %q", got.GitHubAPIURL, "https://api.github.com")
	}
	if got.GitHubDeveloperAppID != 101 || got.GitHubReviewerAppID != 202 {
		t.Errorf("GitHub App IDs = (%d, %d), want (101, 202)", got.GitHubDeveloperAppID, got.GitHubReviewerAppID)
	}
	if got.GitHubDeveloperPrivateKeyFile != "/secrets/developer.pem" || got.GitHubReviewerPrivateKeyFile != "/secrets/reviewer.pem" || got.GitHubWebhookSecretFile != "/secrets/webhook" {
		t.Errorf("GitHub secret files = (%q, %q, %q)", got.GitHubDeveloperPrivateKeyFile, got.GitHubReviewerPrivateKeyFile, got.GitHubWebhookSecretFile)
	}
	if got.WebhookLeaseDuration != 30*time.Second || got.WebhookPollInterval != 250*time.Millisecond {
		t.Errorf("webhook timing = (%s, %s), want (30s, 250ms)", got.WebhookLeaseDuration, got.WebhookPollInterval)
	}
	if got.AssignmentRetentionDuration != 30*24*time.Hour || got.AgentTurnConcurrencyLimit != 2 {
		t.Errorf("workflow limits = (%s, %d), want (720h, 2)", got.AssignmentRetentionDuration, got.AgentTurnConcurrencyLimit)
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
		"OMNIGREX_DATABASE_URL":                      "postgres://app@database/app",
		"OMNIGREX_DATABASE_PASSWORD_SECRET_FILE":     "/secrets/database-password",
		"OMNIGREX_DOCKER_AGENT_NETWORK":              "agents",
		"OMNIGREX_WORKSPACE_VOLUME":                  "workspaces",
		"OMNIGREX_RUNTIME_STATE_VOLUME":              "runtime-state",
		"OMNIGREX_MISE_VOLUME":                       "mise",
		"OMNIGREX_AGENT_IMAGE_REFERENCE":             "registry.example/agent:v2",
		"OMNIGREX_GITHUB_API_URL":                    "https://github.example/api/v3",
		"OMNIGREX_GITHUB_DEVELOPER_APP_ID":           "303",
		"OMNIGREX_GITHUB_REVIEWER_APP_ID":            "404",
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE": "/secrets/custom-developer.pem",
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":  "/secrets/custom-reviewer.pem",
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":        "/secrets/custom-webhook",
		"OMNIGREX_WEBHOOK_LEASE_DURATION":            "45s",
		"OMNIGREX_WEBHOOK_POLL_INTERVAL":             "500ms",
		"OMNIGREX_ASSIGNMENT_RETENTION_DURATION":     "48h",
		"OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT":      "7",
		"OMNIGREX_HTTP_ADDR":                         "127.0.0.1:9000",
		"OMNIGREX_READINESS_TIMEOUT":                 "3s",
		"OMNIGREX_SHUTDOWN_TIMEOUT":                  "20s",
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
	if got.GitHubAPIURL != values["OMNIGREX_GITHUB_API_URL"] || got.GitHubDeveloperAppID != 303 || got.GitHubReviewerAppID != 404 {
		t.Errorf("GitHub API/App config = (%q, %d, %d)", got.GitHubAPIURL, got.GitHubDeveloperAppID, got.GitHubReviewerAppID)
	}
	if got.GitHubDeveloperPrivateKeyFile != values["OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE"] || got.GitHubReviewerPrivateKeyFile != values["OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE"] || got.GitHubWebhookSecretFile != values["OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE"] {
		t.Errorf("GitHub secret file config = (%q, %q, %q)", got.GitHubDeveloperPrivateKeyFile, got.GitHubReviewerPrivateKeyFile, got.GitHubWebhookSecretFile)
	}
	if got.WebhookLeaseDuration != 45*time.Second || got.WebhookPollInterval != 500*time.Millisecond {
		t.Errorf("webhook timing = (%s, %s), want (45s, 500ms)", got.WebhookLeaseDuration, got.WebhookPollInterval)
	}
	if got.AssignmentRetentionDuration != 48*time.Hour || got.AgentTurnConcurrencyLimit != 7 {
		t.Errorf("workflow limits = (%s, %d), want (48h, 7)", got.AssignmentRetentionDuration, got.AgentTurnConcurrencyLimit)
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
		{name: "GitHub API URL", key: "OMNIGREX_GITHUB_API_URL", value: "://invalid"},
		{name: "insecure GitHub API URL", key: "OMNIGREX_GITHUB_API_URL", value: "http://github.example"},
		{name: "Developer App ID", key: "OMNIGREX_GITHUB_DEVELOPER_APP_ID", value: "0"},
		{name: "Reviewer App ID", key: "OMNIGREX_GITHUB_REVIEWER_APP_ID", value: "not-an-id"},
		{name: "Developer private key file", key: "OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE", value: "relative.pem"},
		{name: "Reviewer private key file", key: "OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE", value: "relative.pem"},
		{name: "webhook secret file", key: "OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE", value: "relative"},
		{name: "webhook lease syntax", key: "OMNIGREX_WEBHOOK_LEASE_DURATION", value: "later"},
		{name: "webhook lease value", key: "OMNIGREX_WEBHOOK_LEASE_DURATION", value: "1s"},
		{name: "webhook poll syntax", key: "OMNIGREX_WEBHOOK_POLL_INTERVAL", value: "often"},
		{name: "webhook poll value", key: "OMNIGREX_WEBHOOK_POLL_INTERVAL", value: "-1s"},
		{name: "assignment retention syntax", key: "OMNIGREX_ASSIGNMENT_RETENTION_DURATION", value: "later"},
		{name: "assignment retention value", key: "OMNIGREX_ASSIGNMENT_RETENTION_DURATION", value: "0s"},
		{name: "Agent Turn concurrency syntax", key: "OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT", value: "many"},
		{name: "Agent Turn concurrency value", key: "OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT", value: "0"},
		{name: "readiness timeout syntax", key: "OMNIGREX_READINESS_TIMEOUT", value: "eventually"},
		{name: "readiness timeout value", key: "OMNIGREX_READINESS_TIMEOUT", value: "0s"},
		{name: "shutdown timeout syntax", key: "OMNIGREX_SHUTDOWN_TIMEOUT", value: "eventually"},
		{name: "shutdown timeout value", key: "OMNIGREX_SHUTDOWN_TIMEOUT", value: "-1s"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Load(environment(map[string]string{test.key: test.value}))
			if err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
		})
	}
}

func TestLoadRejectsSameGitHubAppIdentity(t *testing.T) {
	_, err := config.Load(environment(map[string]string{"OMNIGREX_GITHUB_REVIEWER_APP_ID": "101"}))
	if err == nil {
		t.Fatal("Load() error = nil, want distinct Developer and Reviewer App IDs")
	}
}

func environment(overrides map[string]string) func(string) string {
	values := map[string]string{
		"OMNIGREX_GITHUB_DEVELOPER_APP_ID":           "101",
		"OMNIGREX_GITHUB_REVIEWER_APP_ID":            "202",
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE": "/secrets/developer.pem",
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":  "/secrets/reviewer.pem",
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":        "/secrets/webhook",
	}
	for key, value := range overrides {
		values[key] = value
	}
	return func(key string) string { return values[key] }
}
