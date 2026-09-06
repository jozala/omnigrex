package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const deploymentImage = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
	if got.WorkspaceRoot != "/var/lib/omnigrex/workspaces" || got.MiseRoot != "/var/lib/omnigrex/mise" {
		t.Errorf("orchestrator workspace roots = (%q, %q)", got.WorkspaceRoot, got.MiseRoot)
	}
	if got.MCPAddr != "omnigrex-mcp:8081" || got.MCPEndpointURL != "http://omnigrex-mcp:8081/mcp" || got.MCPMutationOperationTimeout != 2*time.Hour {
		t.Errorf("MCP settings = (%q, %q, %s)", got.MCPAddr, got.MCPEndpointURL, got.MCPMutationOperationTimeout)
	}
	if got.AgentImageReference != "omnigrex/opencode:1.18.29" {
		t.Errorf("AgentImageReference = %q, want %q", got.AgentImageReference, "omnigrex/opencode:1.18.29")
	}
	if got.OpenCodeACPV1Image != deploymentImage {
		t.Errorf("OpenCodeACPV1Image = %q, want %q", got.OpenCodeACPV1Image, deploymentImage)
	}
	if got.RuntimeProfileCompatibilityResultsFile != "" {
		t.Errorf("RuntimeProfileCompatibilityResultsFile = %q, want empty", got.RuntimeProfileCompatibilityResultsFile)
	}
	if want := (profile.Platform{OS: "linux", Arch: "arm64"}); !reflect.DeepEqual(got.OpenCodeACPV1Platform, want) {
		t.Errorf("OpenCodeACPV1Platform = %#v, want %#v", got.OpenCodeACPV1Platform, want)
	}
	if got.GitHubAPIURL != "https://api.github.com" {
		t.Errorf("GitHubAPIURL = %q, want %q", got.GitHubAPIURL, "https://api.github.com")
	}
	if got.GitRemoteBaseURL != "https://github.com" {
		t.Errorf("GitRemoteBaseURL = %q, want %q", got.GitRemoteBaseURL, "https://github.com")
	}
	if got.GitHubDeveloperAppID != 101 || got.GitHubReviewerAppID != 202 {
		t.Errorf("GitHub App IDs = (%d, %d), want (101, 202)", got.GitHubDeveloperAppID, got.GitHubReviewerAppID)
	}
	if got.GitHubDeveloperPrivateKeyFile != "/secrets/developer.pem" || got.GitHubReviewerPrivateKeyFile != "/secrets/reviewer.pem" || got.GitHubWebhookSecretFile != "/secrets/webhook" {
		t.Errorf("GitHub secret files = (%q, %q, %q)", got.GitHubDeveloperPrivateKeyFile, got.GitHubReviewerPrivateKeyFile, got.GitHubWebhookSecretFile)
	}
	if got.DeveloperProviderCredentialsFile != "/secrets/developer-provider.json" || got.ReviewerProviderCredentialsFile != "/secrets/reviewer-provider.json" {
		t.Errorf("provider credential files = (%q, %q)", got.DeveloperProviderCredentialsFile, got.ReviewerProviderCredentialsFile)
	}
	if got.WebhookLeaseDuration != 30*time.Second || got.WebhookPollInterval != 250*time.Millisecond {
		t.Errorf("webhook timing = (%s, %s), want (30s, 250ms)", got.WebhookLeaseDuration, got.WebhookPollInterval)
	}
	if got.AgentTurnPreparationLeaseDuration != 30*time.Second || got.AgentTurnPreparationHeartbeatInterval != 10*time.Second || got.AgentTurnPreparationPollInterval != 250*time.Millisecond || got.AgentTurnPreparationRetryDelay != 5*time.Second {
		t.Errorf("Agent Turn preparation timing = (%s, %s, %s, %s), want (30s, 10s, 250ms, 5s)", got.AgentTurnPreparationLeaseDuration, got.AgentTurnPreparationHeartbeatInterval, got.AgentTurnPreparationPollInterval, got.AgentTurnPreparationRetryDelay)
	}
	if got.AgentTurnExecutionLeaseDuration != 30*time.Second || got.AgentTurnExecutionHeartbeatInterval != 10*time.Second || got.AgentTurnExecutionPollInterval != 250*time.Millisecond || got.AgentTurnExecutionTurnTimeout != 2*time.Hour || got.AgentTurnExecutionCleanupTimeout != 10*time.Second {
		t.Errorf("Agent Turn execution timing = (%s, %s, %s, %s, %s), want (30s, 10s, 250ms, 2h, 10s)", got.AgentTurnExecutionLeaseDuration, got.AgentTurnExecutionHeartbeatInterval, got.AgentTurnExecutionPollInterval, got.AgentTurnExecutionTurnTimeout, got.AgentTurnExecutionCleanupTimeout)
	}
	if got.WorkflowEffectLeaseDuration != 30*time.Second || got.WorkflowEffectHeartbeatInterval != 10*time.Second || got.WorkflowEffectPollInterval != 250*time.Millisecond || got.WorkflowEffectRetryDelay != 5*time.Second {
		t.Errorf("Workflow effect timing = (%s, %s, %s, %s), want (30s, 10s, 250ms, 5s)", got.WorkflowEffectLeaseDuration, got.WorkflowEffectHeartbeatInterval, got.WorkflowEffectPollInterval, got.WorkflowEffectRetryDelay)
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
		"OMNIGREX_DATABASE_URL":                               "postgres://app@database/app",
		"OMNIGREX_DATABASE_PASSWORD_SECRET_FILE":              "/secrets/database-password",
		"OMNIGREX_DOCKER_AGENT_NETWORK":                       "agents",
		"OMNIGREX_WORKSPACE_VOLUME":                           "workspaces",
		"OMNIGREX_RUNTIME_STATE_VOLUME":                       "runtime-state",
		"OMNIGREX_MISE_VOLUME":                                "mise",
		"OMNIGREX_WORKSPACE_ROOT":                             "/data/workspaces",
		"OMNIGREX_MISE_ROOT":                                  "/data/mise",
		"OMNIGREX_MCP_ADDR":                                   "127.0.0.1:9001",
		"OMNIGREX_MCP_ENDPOINT_URL":                           "https://mcp.internal/mcp",
		"OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT":             "17m",
		"OMNIGREX_AGENT_IMAGE_REFERENCE":                      "registry.example/agent:v2",
		"OMNIGREX_OPENCODE_ACP_V1_IMAGE":                      "registry.example/agent/opencode@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"OMNIGREX_OPENCODE_ACP_V1_PLATFORM":                   "linux/amd64",
		"OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE": "/results/runtime-profile-compatibility.json",
		"OMNIGREX_GITHUB_API_URL":                             "https://github.example/api/v3",
		"OMNIGREX_GIT_REMOTE_BASE_URL":                        "https://github.example/source",
		"OMNIGREX_GITHUB_DEVELOPER_APP_ID":                    "303",
		"OMNIGREX_GITHUB_REVIEWER_APP_ID":                     "404",
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE":          "/secrets/custom-developer.pem",
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":           "/secrets/custom-reviewer.pem",
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":                 "/secrets/custom-webhook",
		"OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE":        "/secrets/custom-developer-provider.json",
		"OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE":         "/secrets/custom-reviewer-provider.json",
		"OMNIGREX_WEBHOOK_LEASE_DURATION":                     "45s",
		"OMNIGREX_WEBHOOK_POLL_INTERVAL":                      "500ms",
		"OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION":      "1m",
		"OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL":  "15s",
		"OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL":       "750ms",
		"OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY":         "3s",
		"OMNIGREX_AGENT_TURN_EXECUTION_LEASE_DURATION":        "45s",
		"OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL":    "15s",
		"OMNIGREX_AGENT_TURN_EXECUTION_POLL_INTERVAL":         "400ms",
		"OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT":          "3h",
		"OMNIGREX_AGENT_TURN_EXECUTION_CLEANUP_TIMEOUT":       "12s",
		"OMNIGREX_WORKFLOW_EFFECT_LEASE_DURATION":             "1m",
		"OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL":         "20s",
		"OMNIGREX_WORKFLOW_EFFECT_POLL_INTERVAL":              "600ms",
		"OMNIGREX_WORKFLOW_EFFECT_RETRY_DELAY":                "4s",
		"OMNIGREX_ASSIGNMENT_RETENTION_DURATION":              "48h",
		"OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT":               "7",
		"OMNIGREX_HTTP_ADDR":                                  "127.0.0.1:9000",
		"OMNIGREX_READINESS_TIMEOUT":                          "3s",
		"OMNIGREX_SHUTDOWN_TIMEOUT":                           "20s",
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
	if got.WorkspaceRoot != values["OMNIGREX_WORKSPACE_ROOT"] || got.MiseRoot != values["OMNIGREX_MISE_ROOT"] ||
		got.MCPAddr != values["OMNIGREX_MCP_ADDR"] || got.MCPEndpointURL != values["OMNIGREX_MCP_ENDPOINT_URL"] || got.MCPMutationOperationTimeout != 17*time.Minute {
		t.Errorf("workspace/MCP settings = (%q, %q, %q, %q, %s)", got.WorkspaceRoot, got.MiseRoot, got.MCPAddr, got.MCPEndpointURL, got.MCPMutationOperationTimeout)
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
	if got.OpenCodeACPV1Image != values["OMNIGREX_OPENCODE_ACP_V1_IMAGE"] || got.OpenCodeACPV1Platform != (profile.Platform{OS: "linux", Arch: "amd64"}) {
		t.Errorf("OpenCode ACP v1 deployment = (%q, %#v)", got.OpenCodeACPV1Image, got.OpenCodeACPV1Platform)
	}
	if got.RuntimeProfileCompatibilityResultsFile != values["OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE"] {
		t.Errorf("RuntimeProfileCompatibilityResultsFile = %q", got.RuntimeProfileCompatibilityResultsFile)
	}
	if got.GitHubAPIURL != values["OMNIGREX_GITHUB_API_URL"] || got.GitHubDeveloperAppID != 303 || got.GitHubReviewerAppID != 404 {
		t.Errorf("GitHub API/App config = (%q, %d, %d)", got.GitHubAPIURL, got.GitHubDeveloperAppID, got.GitHubReviewerAppID)
	}
	if got.GitRemoteBaseURL != values["OMNIGREX_GIT_REMOTE_BASE_URL"] {
		t.Errorf("GitRemoteBaseURL = %q, want %q", got.GitRemoteBaseURL, values["OMNIGREX_GIT_REMOTE_BASE_URL"])
	}
	if got.GitHubDeveloperPrivateKeyFile != values["OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE"] || got.GitHubReviewerPrivateKeyFile != values["OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE"] || got.GitHubWebhookSecretFile != values["OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE"] {
		t.Errorf("GitHub secret file config = (%q, %q, %q)", got.GitHubDeveloperPrivateKeyFile, got.GitHubReviewerPrivateKeyFile, got.GitHubWebhookSecretFile)
	}
	if got.DeveloperProviderCredentialsFile != values["OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE"] || got.ReviewerProviderCredentialsFile != values["OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE"] {
		t.Errorf("provider credential file config = (%q, %q)", got.DeveloperProviderCredentialsFile, got.ReviewerProviderCredentialsFile)
	}
	if got.WebhookLeaseDuration != 45*time.Second || got.WebhookPollInterval != 500*time.Millisecond {
		t.Errorf("webhook timing = (%s, %s), want (45s, 500ms)", got.WebhookLeaseDuration, got.WebhookPollInterval)
	}
	if got.AgentTurnPreparationLeaseDuration != time.Minute || got.AgentTurnPreparationHeartbeatInterval != 15*time.Second || got.AgentTurnPreparationPollInterval != 750*time.Millisecond || got.AgentTurnPreparationRetryDelay != 3*time.Second {
		t.Errorf("Agent Turn preparation timing = (%s, %s, %s, %s)", got.AgentTurnPreparationLeaseDuration, got.AgentTurnPreparationHeartbeatInterval, got.AgentTurnPreparationPollInterval, got.AgentTurnPreparationRetryDelay)
	}
	if got.AgentTurnExecutionLeaseDuration != 45*time.Second || got.AgentTurnExecutionHeartbeatInterval != 15*time.Second || got.AgentTurnExecutionPollInterval != 400*time.Millisecond || got.AgentTurnExecutionTurnTimeout != 3*time.Hour || got.AgentTurnExecutionCleanupTimeout != 12*time.Second {
		t.Errorf("Agent Turn execution timing = (%s, %s, %s, %s, %s)", got.AgentTurnExecutionLeaseDuration, got.AgentTurnExecutionHeartbeatInterval, got.AgentTurnExecutionPollInterval, got.AgentTurnExecutionTurnTimeout, got.AgentTurnExecutionCleanupTimeout)
	}
	if got.WorkflowEffectLeaseDuration != time.Minute || got.WorkflowEffectHeartbeatInterval != 20*time.Second || got.WorkflowEffectPollInterval != 600*time.Millisecond || got.WorkflowEffectRetryDelay != 4*time.Second {
		t.Errorf("Workflow effect timing = (%s, %s, %s, %s)", got.WorkflowEffectLeaseDuration, got.WorkflowEffectHeartbeatInterval, got.WorkflowEffectPollInterval, got.WorkflowEffectRetryDelay)
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
		{name: "missing deployment image", key: "OMNIGREX_OPENCODE_ACP_V1_IMAGE", value: ""},
		{name: "mutable deployment image", key: "OMNIGREX_OPENCODE_ACP_V1_IMAGE", value: "registry.example/agent/opencode:1.18.19"},
		{name: "local deployment image ID", key: "OMNIGREX_OPENCODE_ACP_V1_IMAGE", value: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{name: "unsupported deployment OS", key: "OMNIGREX_OPENCODE_ACP_V1_PLATFORM", value: "darwin/arm64"},
		{name: "unsupported deployment architecture", key: "OMNIGREX_OPENCODE_ACP_V1_PLATFORM", value: "linux/s390x"},
		{name: "missing deployment platform", key: "OMNIGREX_OPENCODE_ACP_V1_PLATFORM", value: ""},
		{name: "malformed deployment platform", key: "OMNIGREX_OPENCODE_ACP_V1_PLATFORM", value: "linux"},
		{name: "compatibility results file", key: "OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE", value: "relative.json"},
		{name: "GitHub API URL", key: "OMNIGREX_GITHUB_API_URL", value: "://invalid"},
		{name: "insecure GitHub API URL", key: "OMNIGREX_GITHUB_API_URL", value: "http://github.example"},
		{name: "insecure Git remote base URL", key: "OMNIGREX_GIT_REMOTE_BASE_URL", value: "http://github.example"},
		{name: "credentialed Git remote base URL", key: "OMNIGREX_GIT_REMOTE_BASE_URL", value: "https://credential-sentinel@github.example"},
		{name: "unclean Git remote base path", key: "OMNIGREX_GIT_REMOTE_BASE_URL", value: "https://github.example/source/../repos"},
		{name: "Developer App ID", key: "OMNIGREX_GITHUB_DEVELOPER_APP_ID", value: "0"},
		{name: "Reviewer App ID", key: "OMNIGREX_GITHUB_REVIEWER_APP_ID", value: "not-an-id"},
		{name: "Developer private key file", key: "OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE", value: "relative.pem"},
		{name: "Reviewer private key file", key: "OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE", value: "relative.pem"},
		{name: "webhook secret file", key: "OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE", value: "relative"},
		{name: "Developer provider credentials file", key: "OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE", value: "relative.json"},
		{name: "Reviewer provider credentials file", key: "OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE", value: "relative.json"},
		{name: "webhook lease syntax", key: "OMNIGREX_WEBHOOK_LEASE_DURATION", value: "later"},
		{name: "webhook lease value", key: "OMNIGREX_WEBHOOK_LEASE_DURATION", value: "1s"},
		{name: "webhook poll syntax", key: "OMNIGREX_WEBHOOK_POLL_INTERVAL", value: "often"},
		{name: "webhook poll value", key: "OMNIGREX_WEBHOOK_POLL_INTERVAL", value: "-1s"},
		{name: "preparation lease syntax", key: "OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION", value: "later"},
		{name: "preparation lease value", key: "OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION", value: "0s"},
		{name: "preparation lease below Store precision", key: "OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION", value: "1ns"},
		{name: "preparation lease above Store maximum", key: "OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION", value: "8760h1us"},
		{name: "preparation heartbeat syntax", key: "OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL", value: "often"},
		{name: "preparation heartbeat value", key: "OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL", value: "0s"},
		{name: "preparation heartbeat below Store precision", key: "OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL", value: "1ns"},
		{name: "preparation heartbeat above Store maximum", key: "OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL", value: "8760h1us"},
		{name: "preparation heartbeat reaches lease", key: "OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL", value: "30s"},
		{name: "preparation poll syntax", key: "OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL", value: "often"},
		{name: "preparation poll value", key: "OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL", value: "0s"},
		{name: "preparation poll below Store precision", key: "OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL", value: "1ns"},
		{name: "preparation poll above Worker maximum", key: "OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL", value: "8760h1us"},
		{name: "preparation retry syntax", key: "OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY", value: "later"},
		{name: "preparation retry value", key: "OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY", value: "0s"},
		{name: "preparation retry below Store precision", key: "OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY", value: "1ns"},
		{name: "preparation retry above Store maximum", key: "OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY", value: "8760h1us"},
		{name: "execution lease value", key: "OMNIGREX_AGENT_TURN_EXECUTION_LEASE_DURATION", value: "0s"},
		{name: "execution heartbeat value", key: "OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL", value: "0s"},
		{name: "execution heartbeat reaches lease", key: "OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL", value: "30s"},
		{name: "execution poll value", key: "OMNIGREX_AGENT_TURN_EXECUTION_POLL_INTERVAL", value: "0s"},
		{name: "execution turn timeout syntax", key: "OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT", value: "eventually"},
		{name: "execution turn timeout above maximum", key: "OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT", value: "8760h1us"},
		{name: "execution cleanup value", key: "OMNIGREX_AGENT_TURN_EXECUTION_CLEANUP_TIMEOUT", value: "0s"},
		{name: "MCP mutation operation timeout syntax", key: "OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT", value: "eventually"},
		{name: "MCP mutation operation timeout below precision", key: "OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT", value: "1ns"},
		{name: "MCP mutation operation timeout above maximum", key: "OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT", value: "8760h1us"},
		{name: "Workflow effect lease value", key: "OMNIGREX_WORKFLOW_EFFECT_LEASE_DURATION", value: "0s"},
		{name: "Workflow effect heartbeat value", key: "OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL", value: "0s"},
		{name: "Workflow effect heartbeat reaches lease", key: "OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL", value: "30s"},
		{name: "Workflow effect poll value", key: "OMNIGREX_WORKFLOW_EFFECT_POLL_INTERVAL", value: "0s"},
		{name: "Workflow effect retry above maximum", key: "OMNIGREX_WORKFLOW_EFFECT_RETRY_DELAY", value: "8760h1us"},
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
			if strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("Load() error disclosed credentials: %v", err)
			}
		})
	}
}

func TestLoadAcceptsAgentTurnPreparationDurationBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides map[string]string
	}{
		{name: "minimum", overrides: map[string]string{
			"OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT":            "1us",
			"OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION":     "2us",
			"OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL": "1us",
			"OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL":      "1us",
			"OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY":        "1us",
		}},
		{name: "maximum", overrides: map[string]string{
			"OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT":            "8760h",
			"OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION":     "8760h",
			"OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL": "8759h59m59.999999s",
			"OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL":      "8760h",
			"OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY":        "8760h",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(environment(test.overrides)); err != nil {
				t.Fatalf("Load() boundary error = %v", err)
			}
		})
	}
}

func TestLoadAcceptsPhase8WorkerDurationBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides map[string]string
	}{
		{name: "minimum", overrides: map[string]string{
			"OMNIGREX_AGENT_TURN_EXECUTION_LEASE_DURATION":     "2us",
			"OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL": "1us",
			"OMNIGREX_AGENT_TURN_EXECUTION_POLL_INTERVAL":      "1us",
			"OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT":       "1us",
			"OMNIGREX_AGENT_TURN_EXECUTION_CLEANUP_TIMEOUT":    "1us",
			"OMNIGREX_WORKFLOW_EFFECT_LEASE_DURATION":          "2us",
			"OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL":      "1us",
			"OMNIGREX_WORKFLOW_EFFECT_POLL_INTERVAL":           "1us",
			"OMNIGREX_WORKFLOW_EFFECT_RETRY_DELAY":             "1us",
		}},
		{name: "maximum", overrides: map[string]string{
			"OMNIGREX_AGENT_TURN_EXECUTION_LEASE_DURATION":     "8760h",
			"OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL": "8759h59m59.999999s",
			"OMNIGREX_AGENT_TURN_EXECUTION_POLL_INTERVAL":      "8760h",
			"OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT":       "8760h",
			"OMNIGREX_AGENT_TURN_EXECUTION_CLEANUP_TIMEOUT":    "8760h",
			"OMNIGREX_WORKFLOW_EFFECT_LEASE_DURATION":          "8760h",
			"OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL":      "8759h59m59.999999s",
			"OMNIGREX_WORKFLOW_EFFECT_POLL_INTERVAL":           "8760h",
			"OMNIGREX_WORKFLOW_EFFECT_RETRY_DELAY":             "8760h",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(environment(test.overrides)); err != nil {
				t.Fatalf("Load() boundary error = %v", err)
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
		"OMNIGREX_OPENCODE_ACP_V1_IMAGE":               deploymentImage,
		"OMNIGREX_OPENCODE_ACP_V1_PLATFORM":            "linux/arm64",
		"OMNIGREX_GITHUB_DEVELOPER_APP_ID":             "101",
		"OMNIGREX_GITHUB_REVIEWER_APP_ID":              "202",
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE":   "/secrets/developer.pem",
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":    "/secrets/reviewer.pem",
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":          "/secrets/webhook",
		"OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE": "/secrets/developer-provider.json",
		"OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE":  "/secrets/reviewer-provider.json",
	}
	for key, value := range overrides {
		values[key] = value
	}
	return func(key string) string { return values[key] }
}
