package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/gitremote"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const (
	defaultDatabaseURL                 = "postgres://omnigrex@postgres:5432/omnigrex?sslmode=disable"
	defaultDatabasePasswordSecretFile  = "/run/secrets/omnigrex-database-password"
	defaultDockerAgentNetwork          = "omnigrex-agent"
	defaultWorkspaceVolume             = "omnigrex-workspaces"
	defaultRuntimeStateVolume          = "omnigrex-runtime-state"
	defaultMiseVolume                  = "omnigrex-mise"
	defaultWorkspaceRoot               = "/var/lib/omnigrex/workspaces"
	defaultMiseRoot                    = "/var/lib/omnigrex/mise"
	defaultMCPAddr                     = "omnigrex-mcp:8081"
	defaultMCPEndpointURL              = "http://omnigrex-mcp:8081/mcp"
	defaultMCPMutationOperationTimeout = 2 * time.Hour
	defaultAgentImageReference         = "omnigrex/opencode:1.18.19"
	defaultGitHubAPIURL                = "https://api.github.com"
	defaultGitRemoteBaseURL            = gitremote.DefaultBaseURL
	defaultHTTPAddr                    = ":8080"
	defaultReadinessTimeout            = 15 * time.Second
	defaultShutdownTimeout             = 10 * time.Second
	defaultWebhookLeaseDuration        = 30 * time.Second
	defaultWebhookPollInterval         = 250 * time.Millisecond
	defaultPreparationLeaseDuration    = 30 * time.Second
	defaultPreparationHeartbeat        = 10 * time.Second
	defaultPreparationPollInterval     = 250 * time.Millisecond
	defaultPreparationRetryDelay       = 5 * time.Second
	defaultExecutionLeaseDuration      = 30 * time.Second
	defaultExecutionHeartbeat          = 10 * time.Second
	defaultExecutionPollInterval       = 250 * time.Millisecond
	defaultExecutionTurnTimeout        = 2 * time.Hour
	defaultExecutionCleanupTimeout     = 10 * time.Second
	defaultWorkflowEffectLeaseDuration = 30 * time.Second
	defaultWorkflowEffectHeartbeat     = 10 * time.Second
	defaultWorkflowEffectPollInterval  = 250 * time.Millisecond
	defaultWorkflowEffectRetryDelay    = 5 * time.Second
	defaultAssignmentRetention         = 30 * 24 * time.Hour
	defaultAgentTurnConcurrencyLimit   = 2
	minimumWebhookLeaseDuration        = 5 * time.Second
	maximumWorkerDuration              = 365 * 24 * time.Hour
)

type Config struct {
	DatabaseURL                 string
	DatabasePasswordSecretFile  string
	DockerAgentNetwork          string
	WorkspaceVolume             string
	RuntimeStateVolume          string
	MiseVolume                  string
	WorkspaceRoot               string
	MiseRoot                    string
	MCPAddr                     string
	MCPEndpointURL              string
	MCPMutationOperationTimeout time.Duration
	// AgentImageReference is the mutable local image used by deployment readiness checks.
	AgentImageReference string
	// OpenCodeACPV1Image and OpenCodeACPV1Platform define the immutable deployment Runtime Profile.
	OpenCodeACPV1Image                     string
	OpenCodeACPV1Platform                  profile.Platform
	RuntimeProfileCompatibilityResultsFile string
	GitHubAPIURL                           string
	GitRemoteBaseURL                       string
	GitHubDeveloperAppID                   int64
	GitHubReviewerAppID                    int64
	GitHubDeveloperPrivateKeyFile          string
	GitHubReviewerPrivateKeyFile           string
	GitHubWebhookSecretFile                string
	DeveloperProviderCredentialsFile       string
	ReviewerProviderCredentialsFile        string
	WebhookLeaseDuration                   time.Duration
	WebhookPollInterval                    time.Duration
	AgentTurnPreparationLeaseDuration      time.Duration
	AgentTurnPreparationHeartbeatInterval  time.Duration
	AgentTurnPreparationPollInterval       time.Duration
	AgentTurnPreparationRetryDelay         time.Duration
	AgentTurnExecutionLeaseDuration        time.Duration
	AgentTurnExecutionHeartbeatInterval    time.Duration
	AgentTurnExecutionPollInterval         time.Duration
	AgentTurnExecutionTurnTimeout          time.Duration
	AgentTurnExecutionCleanupTimeout       time.Duration
	WorkflowEffectLeaseDuration            time.Duration
	WorkflowEffectHeartbeatInterval        time.Duration
	WorkflowEffectPollInterval             time.Duration
	WorkflowEffectRetryDelay               time.Duration
	AssignmentRetentionDuration            time.Duration
	AgentTurnConcurrencyLimit              int
	HTTPAddr                               string
	ReadinessTimeout                       time.Duration
	ShutdownTimeout                        time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	config := Config{
		DatabaseURL:                            valueOrDefault(getenv("OMNIGREX_DATABASE_URL"), defaultDatabaseURL),
		DatabasePasswordSecretFile:             valueOrDefault(getenv("OMNIGREX_DATABASE_PASSWORD_SECRET_FILE"), defaultDatabasePasswordSecretFile),
		DockerAgentNetwork:                     valueOrDefault(getenv("OMNIGREX_DOCKER_AGENT_NETWORK"), defaultDockerAgentNetwork),
		WorkspaceVolume:                        valueOrDefault(getenv("OMNIGREX_WORKSPACE_VOLUME"), defaultWorkspaceVolume),
		RuntimeStateVolume:                     valueOrDefault(getenv("OMNIGREX_RUNTIME_STATE_VOLUME"), defaultRuntimeStateVolume),
		MiseVolume:                             valueOrDefault(getenv("OMNIGREX_MISE_VOLUME"), defaultMiseVolume),
		WorkspaceRoot:                          valueOrDefault(getenv("OMNIGREX_WORKSPACE_ROOT"), defaultWorkspaceRoot),
		MiseRoot:                               valueOrDefault(getenv("OMNIGREX_MISE_ROOT"), defaultMiseRoot),
		MCPAddr:                                valueOrDefault(getenv("OMNIGREX_MCP_ADDR"), defaultMCPAddr),
		MCPEndpointURL:                         valueOrDefault(getenv("OMNIGREX_MCP_ENDPOINT_URL"), defaultMCPEndpointURL),
		MCPMutationOperationTimeout:            defaultMCPMutationOperationTimeout,
		AgentImageReference:                    valueOrDefault(getenv("OMNIGREX_AGENT_IMAGE_REFERENCE"), defaultAgentImageReference),
		OpenCodeACPV1Image:                     getenv("OMNIGREX_OPENCODE_ACP_V1_IMAGE"),
		RuntimeProfileCompatibilityResultsFile: getenv("OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE"),
		GitHubAPIURL:                           valueOrDefault(getenv("OMNIGREX_GITHUB_API_URL"), defaultGitHubAPIURL),
		GitRemoteBaseURL:                       valueOrDefault(getenv("OMNIGREX_GIT_REMOTE_BASE_URL"), defaultGitRemoteBaseURL),
		GitHubDeveloperPrivateKeyFile:          getenv("OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE"),
		GitHubReviewerPrivateKeyFile:           getenv("OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE"),
		GitHubWebhookSecretFile:                getenv("OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE"),
		DeveloperProviderCredentialsFile:       getenv("OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE"),
		ReviewerProviderCredentialsFile:        getenv("OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE"),
		WebhookLeaseDuration:                   defaultWebhookLeaseDuration,
		WebhookPollInterval:                    defaultWebhookPollInterval,
		AgentTurnPreparationLeaseDuration:      defaultPreparationLeaseDuration,
		AgentTurnPreparationHeartbeatInterval:  defaultPreparationHeartbeat,
		AgentTurnPreparationPollInterval:       defaultPreparationPollInterval,
		AgentTurnPreparationRetryDelay:         defaultPreparationRetryDelay,
		AgentTurnExecutionLeaseDuration:        defaultExecutionLeaseDuration,
		AgentTurnExecutionHeartbeatInterval:    defaultExecutionHeartbeat,
		AgentTurnExecutionPollInterval:         defaultExecutionPollInterval,
		AgentTurnExecutionTurnTimeout:          defaultExecutionTurnTimeout,
		AgentTurnExecutionCleanupTimeout:       defaultExecutionCleanupTimeout,
		WorkflowEffectLeaseDuration:            defaultWorkflowEffectLeaseDuration,
		WorkflowEffectHeartbeatInterval:        defaultWorkflowEffectHeartbeat,
		WorkflowEffectPollInterval:             defaultWorkflowEffectPollInterval,
		WorkflowEffectRetryDelay:               defaultWorkflowEffectRetryDelay,
		AssignmentRetentionDuration:            defaultAssignmentRetention,
		AgentTurnConcurrencyLimit:              defaultAgentTurnConcurrencyLimit,
		HTTPAddr:                               valueOrDefault(getenv("OMNIGREX_HTTP_ADDR"), defaultHTTPAddr),
		ReadinessTimeout:                       defaultReadinessTimeout,
		ShutdownTimeout:                        defaultShutdownTimeout,
	}

	databaseURL, err := url.Parse(config.DatabaseURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") {
		return Config{}, fmt.Errorf("OMNIGREX_DATABASE_URL must be a PostgreSQL URL")
	}
	if !filepath.IsAbs(config.DatabasePasswordSecretFile) {
		return Config{}, fmt.Errorf("OMNIGREX_DATABASE_PASSWORD_SECRET_FILE must be an absolute path")
	}
	for name, root := range map[string]string{
		"OMNIGREX_WORKSPACE_ROOT": config.WorkspaceRoot,
		"OMNIGREX_MISE_ROOT":      config.MiseRoot,
	} {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return Config{}, fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	mcpEndpoint, err := url.Parse(config.MCPEndpointURL)
	if err != nil || (mcpEndpoint.Scheme != "http" && mcpEndpoint.Scheme != "https") || mcpEndpoint.Host == "" ||
		mcpEndpoint.User != nil || mcpEndpoint.RawQuery != "" || mcpEndpoint.Fragment != "" || mcpEndpoint.Path != "/mcp" {
		return Config{}, fmt.Errorf("OMNIGREX_MCP_ENDPOINT_URL must be an HTTP URL ending at /mcp without credentials, query, or fragment")
	}
	if strings.TrimSpace(config.MCPAddr) == "" {
		return Config{}, fmt.Errorf("OMNIGREX_MCP_ADDR must not be blank")
	}
	platformOS, platformArch, found := strings.Cut(getenv("OMNIGREX_OPENCODE_ACP_V1_PLATFORM"), "/")
	if !found || strings.Contains(platformArch, "/") {
		return Config{}, fmt.Errorf("OMNIGREX_OPENCODE_ACP_V1_PLATFORM must be a supported linux platform")
	}
	config.OpenCodeACPV1Platform = profile.Platform{OS: platformOS, Arch: platformArch}
	if _, err := profile.NewOpenCodeV1(config.OpenCodeACPV1Image, config.OpenCodeACPV1Platform); err != nil {
		return Config{}, fmt.Errorf("invalid opencode-acp/v1 deployment settings: %w", err)
	}
	if config.RuntimeProfileCompatibilityResultsFile != "" &&
		(!filepath.IsAbs(config.RuntimeProfileCompatibilityResultsFile) || filepath.Clean(config.RuntimeProfileCompatibilityResultsFile) != config.RuntimeProfileCompatibilityResultsFile) {
		return Config{}, fmt.Errorf("OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE must be a clean absolute path")
	}
	githubAPIURL, err := url.Parse(config.GitHubAPIURL)
	if err != nil || githubAPIURL.Scheme != "https" || githubAPIURL.Host == "" || githubAPIURL.RawQuery != "" || githubAPIURL.Fragment != "" {
		return Config{}, fmt.Errorf("OMNIGREX_GITHUB_API_URL must be an HTTPS URL without query or fragment")
	}
	gitRemoteBaseURL, err := gitremote.ParseBaseURL(config.GitRemoteBaseURL)
	if err != nil {
		return Config{}, fmt.Errorf("OMNIGREX_GIT_REMOTE_BASE_URL must be a clean HTTPS base URL without credentials, query, or fragment")
	}
	config.GitRemoteBaseURL = gitRemoteBaseURL.String()
	config.GitHubDeveloperAppID, err = positiveInt64(getenv("OMNIGREX_GITHUB_DEVELOPER_APP_ID"))
	if err != nil {
		return Config{}, fmt.Errorf("OMNIGREX_GITHUB_DEVELOPER_APP_ID must be a positive integer")
	}
	config.GitHubReviewerAppID, err = positiveInt64(getenv("OMNIGREX_GITHUB_REVIEWER_APP_ID"))
	if err != nil {
		return Config{}, fmt.Errorf("OMNIGREX_GITHUB_REVIEWER_APP_ID must be a positive integer")
	}
	if config.GitHubDeveloperAppID == config.GitHubReviewerAppID {
		return Config{}, fmt.Errorf("Developer and Reviewer GitHub App IDs must be distinct")
	}
	for name, path := range map[string]string{
		"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE":   config.GitHubDeveloperPrivateKeyFile,
		"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":    config.GitHubReviewerPrivateKeyFile,
		"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":          config.GitHubWebhookSecretFile,
		"OMNIGREX_DEVELOPER_PROVIDER_CREDENTIALS_FILE": config.DeveloperProviderCredentialsFile,
		"OMNIGREX_REVIEWER_PROVIDER_CREDENTIALS_FILE":  config.ReviewerProviderCredentialsFile,
	} {
		if !filepath.IsAbs(path) {
			return Config{}, fmt.Errorf("%s must be an absolute path", name)
		}
	}
	for name, value := range map[string]string{
		"OMNIGREX_DOCKER_AGENT_NETWORK":  config.DockerAgentNetwork,
		"OMNIGREX_WORKSPACE_VOLUME":      config.WorkspaceVolume,
		"OMNIGREX_RUNTIME_STATE_VOLUME":  config.RuntimeStateVolume,
		"OMNIGREX_MISE_VOLUME":           config.MiseVolume,
		"OMNIGREX_AGENT_IMAGE_REFERENCE": config.AgentImageReference,
	} {
		if strings.TrimSpace(value) == "" {
			return Config{}, fmt.Errorf("%s must not be blank", name)
		}
	}

	if value := getenv("OMNIGREX_READINESS_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_READINESS_TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_READINESS_TIMEOUT must be positive")
		}
		config.ReadinessTimeout = timeout
	}

	if value := getenv("OMNIGREX_SHUTDOWN_TIMEOUT"); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_SHUTDOWN_TIMEOUT: %w", err)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_SHUTDOWN_TIMEOUT must be positive")
		}
		config.ShutdownTimeout = timeout
	}

	if value := getenv("OMNIGREX_WEBHOOK_LEASE_DURATION"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_WEBHOOK_LEASE_DURATION: %w", err)
		}
		if duration < minimumWebhookLeaseDuration {
			return Config{}, fmt.Errorf("OMNIGREX_WEBHOOK_LEASE_DURATION must be at least %s", minimumWebhookLeaseDuration)
		}
		config.WebhookLeaseDuration = duration
	}

	if value := getenv("OMNIGREX_WEBHOOK_POLL_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_WEBHOOK_POLL_INTERVAL: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_WEBHOOK_POLL_INTERVAL must be positive")
		}
		config.WebhookPollInterval = duration
	}

	for name, destination := range map[string]*time.Duration{
		"OMNIGREX_MCP_MUTATION_OPERATION_TIMEOUT":            &config.MCPMutationOperationTimeout,
		"OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION":     &config.AgentTurnPreparationLeaseDuration,
		"OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL": &config.AgentTurnPreparationHeartbeatInterval,
		"OMNIGREX_AGENT_TURN_PREPARATION_POLL_INTERVAL":      &config.AgentTurnPreparationPollInterval,
		"OMNIGREX_AGENT_TURN_PREPARATION_RETRY_DELAY":        &config.AgentTurnPreparationRetryDelay,
		"OMNIGREX_AGENT_TURN_EXECUTION_LEASE_DURATION":       &config.AgentTurnExecutionLeaseDuration,
		"OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL":   &config.AgentTurnExecutionHeartbeatInterval,
		"OMNIGREX_AGENT_TURN_EXECUTION_POLL_INTERVAL":        &config.AgentTurnExecutionPollInterval,
		"OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT":         &config.AgentTurnExecutionTurnTimeout,
		"OMNIGREX_AGENT_TURN_EXECUTION_CLEANUP_TIMEOUT":      &config.AgentTurnExecutionCleanupTimeout,
		"OMNIGREX_WORKFLOW_EFFECT_LEASE_DURATION":            &config.WorkflowEffectLeaseDuration,
		"OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL":        &config.WorkflowEffectHeartbeatInterval,
		"OMNIGREX_WORKFLOW_EFFECT_POLL_INTERVAL":             &config.WorkflowEffectPollInterval,
		"OMNIGREX_WORKFLOW_EFFECT_RETRY_DELAY":               &config.WorkflowEffectRetryDelay,
	} {
		value := getenv(name)
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", name, err)
		}
		if duration < time.Microsecond || duration > maximumWorkerDuration {
			return Config{}, fmt.Errorf("%s must be between one microsecond and %s", name, maximumWorkerDuration)
		}
		*destination = duration
	}
	if config.AgentTurnPreparationHeartbeatInterval >= config.AgentTurnPreparationLeaseDuration {
		return Config{}, fmt.Errorf("OMNIGREX_AGENT_TURN_PREPARATION_HEARTBEAT_INTERVAL must be shorter than OMNIGREX_AGENT_TURN_PREPARATION_LEASE_DURATION")
	}
	if config.AgentTurnExecutionHeartbeatInterval >= config.AgentTurnExecutionLeaseDuration {
		return Config{}, fmt.Errorf("OMNIGREX_AGENT_TURN_EXECUTION_HEARTBEAT_INTERVAL must be shorter than OMNIGREX_AGENT_TURN_EXECUTION_LEASE_DURATION")
	}
	if config.WorkflowEffectHeartbeatInterval >= config.WorkflowEffectLeaseDuration {
		return Config{}, fmt.Errorf("OMNIGREX_WORKFLOW_EFFECT_HEARTBEAT_INTERVAL must be shorter than OMNIGREX_WORKFLOW_EFFECT_LEASE_DURATION")
	}

	if value := getenv("OMNIGREX_ASSIGNMENT_RETENTION_DURATION"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse OMNIGREX_ASSIGNMENT_RETENTION_DURATION: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("OMNIGREX_ASSIGNMENT_RETENTION_DURATION must be positive")
		}
		config.AssignmentRetentionDuration = duration
	}

	if value := getenv("OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit <= 0 || strconv.Itoa(limit) != value {
			return Config{}, fmt.Errorf("OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT must be a positive integer")
		}
		config.AgentTurnConcurrencyLimit = limit
	}

	return config, nil
}

func positiveInt64(value string) (int64, error) {
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != value {
		return 0, fmt.Errorf("not a canonical positive integer")
	}
	return number, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
