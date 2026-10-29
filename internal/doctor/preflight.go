package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/config"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/providerauth"
	"github.com/jozala/omnigrex/internal/reviewpolicy"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type productionState struct {
	settings config.Config
	repo     Repository

	runtimeProfile             runtimeprofile.Profile
	runtimeProfiles            []runtimeprofile.Profile
	registry                   runtimeprofile.Registry
	database                   *store.ReadOnlyStore
	api                        *githubapi.APIClient
	developer                  *githubapi.AppJWTSigner
	reviewer                   *githubapi.AppJWTSigner
	developerToken             string
	reviewerToken              string
	profiles                   agentprofile.Snapshot
	profileSelection           agentprofile.Selection
	profilesLoaded             bool
	effectiveProfilesLoaded    bool
	operatorInstructions       agentinstructions.Operator
	operatorInstructionsLoaded bool
}

// RunProduction runs read-only deployment and repository checks in deterministic order.
// GitHub installation-token creation is the only non-GET external request; it does not mutate repository content.
func RunProduction(ctx context.Context, settings config.Config, repository Repository) ([]Result, error) {
	state := &productionState{settings: settings, repo: repository}
	defer state.close()
	checks := []Check{
		{Name: "operator-agent-instructions", Run: state.checkOperatorInstructions},
		{Name: "runtime-profile-contract", Run: state.checkRuntimeProfile},
		{Name: "runtime-profile-compatibility-artifact", Run: state.checkCompatibilityArtifact},
		{Name: "provider-credentials", Run: state.checkProviderCredentials},
		{Name: "github-webhook-secret", Run: func(context.Context) error { return validateNonemptyFile(settings.GitHubWebhookSecretFile) }},
		{Name: "developer-github-app-key", Run: state.checkDeveloperKey},
		{Name: "reviewer-github-app-key", Run: state.checkReviewerKey},
		{Name: "github-app-role-separation", Run: state.checkRoleSeparation},
		{Name: "postgres-connectivity", Run: state.checkDatabase},
		{Name: "postgres-schema", Run: state.checkDatabaseSchema},
		{Name: backendResourcesName(settings), Run: state.checkDockerResources},
		{Name: "runtime-profile-image", Run: state.checkRuntimeImage},
		{Name: "pi-runtime", Run: state.checkAgentRuntime},
		{Name: "protected-runtime-profiles", Run: state.checkProtectedRuntimeProfiles},
		{Name: "pending-runtime-profile-compatibility", Run: state.checkPendingRuntimeCompatibility},
		{Name: "developer-github-installation", Run: state.checkDeveloperInstallation},
		{Name: "reviewer-github-installation", Run: state.checkReviewerInstallation},
		{Name: "agent-profiles", Run: state.checkAgentProfiles},
		{Name: "openai-subscription", Run: state.checkOpenAISubscription},
		{Name: "effective-agent-profiles", Run: state.checkEffectiveProfiles},
		{Name: "effective-agent-runtimes", Run: state.checkEffectiveRuntimes},
		{Name: "mcp-endpoint", Run: state.checkMCPEndpoint},
	}
	runner, err := NewRunner(settings.ReadinessTimeout, checks)
	if err != nil {
		return nil, err
	}
	return runner.Run(ctx), nil
}

func (state *productionState) checkProviderCredentials(ctx context.Context) error {
	if state.settings.OpenAIAuthMode != "subscription" {
		return validateJSONObjectFile(state.settings.ProviderCredentialsFile)
	}
	bundle, err := providerauth.SubscriptionBundle(state.settings.ProviderCredentialsFile)
	clear(bundle)
	if err != nil {
		return err
	}
	return nil
}

func (state *productionState) checkOpenAISubscription(ctx context.Context) error {
	if state.settings.OpenAIAuthMode != "subscription" {
		return nil
	}
	if !state.profilesLoaded {
		return errors.New("Agent Profile check did not pass")
	}
	selected := false
	for _, roleID := range role.BuiltinPolicyCatalog().Roles() {
		name, ok := state.profileSelection.Profile(roleID)
		p, found := state.profiles.Profile(name)
		if ok && found && strings.HasPrefix(p.Model(), "openai/") {
			selected = true
		}
	}
	if !selected {
		return nil
	}
	status, err := providerauth.NewAdminClient(state.settings.ProviderAuthRoot).Status(ctx)
	if err != nil {
		return err
	}
	if !status.StorageAvailable {
		return providerauth.ErrStorage
	}
	if !status.SignedIn || status.AuthBlocked {
		return providerauth.ErrAuthentication
	}
	if status.QuotaBlocked {
		return providerauth.ErrQuota
	}
	return nil
}

func backendResourcesName(settings config.Config) string {
	if settings.ExecutionBackend == "microsandbox" {
		return "microsandbox-kvm-resources"
	}
	return "docker-resources"
}

func (state *productionState) close() {
	if state.database != nil {
		state.database.Close()
	}
	state.developerToken = ""
	state.reviewerToken = ""
}

func (state *productionState) checkRuntimeProfile(context.Context) error {
	profiles, err := state.settings.RuntimeProfiles()
	if err != nil {
		return err
	}
	registry, err := runtimeprofile.NewRegistry(profiles...)
	if err != nil {
		return err
	}
	state.runtimeProfile = profiles[0]
	state.runtimeProfiles = profiles
	state.registry = registry
	return nil
}

func (state *productionState) checkOperatorInstructions(context.Context) error {
	operator, err := agentinstructions.Load(state.settings.AgentInstructionsDir, role.BuiltinPolicyCatalog())
	if err != nil {
		return err
	}
	state.operatorInstructions, state.operatorInstructionsLoaded = operator, true
	return nil
}

func (state *productionState) checkDeveloperKey(context.Context) error {
	signer, err := readSigner(state.settings.GitHubDeveloperAppID, state.settings.GitHubDeveloperPrivateKeyFile)
	state.developer = signer
	return err
}

func (state *productionState) checkReviewerKey(context.Context) error {
	signer, err := readSigner(state.settings.GitHubReviewerAppID, state.settings.GitHubReviewerPrivateKeyFile)
	state.reviewer = signer
	return err
}

func (state *productionState) checkRoleSeparation(context.Context) error {
	if state.developer == nil || state.reviewer == nil {
		return errors.New("GitHub App key checks did not pass")
	}
	if state.developer.PublicKeyFingerprint() == state.reviewer.PublicKeyFingerprint() {
		return errors.New("Developer and Reviewer GitHub Apps use the same private key")
	}
	return nil
}

func (state *productionState) checkDatabase(ctx context.Context) error {
	database, err := store.OpenReadOnly(ctx, state.settings.DatabaseURL, state.settings.DatabasePasswordSecretFile)
	if err != nil {
		return err
	}
	state.database = database
	return nil
}

func (state *productionState) checkDatabaseSchema(ctx context.Context) error {
	if state.database == nil {
		return errors.New("PostgreSQL connectivity check did not pass")
	}
	return state.database.CheckMigrations(ctx)
}

func (state *productionState) checkDockerResources(ctx context.Context) error {
	probe, err := composition.NewResourceProbe(composition.Resources{
		AgentNetwork: state.settings.DockerAgentNetwork, WorkspaceVolume: state.settings.WorkspaceVolume,
		RuntimeStateVolume: state.settings.RuntimeStateVolume, MiseVolume: state.settings.MiseVolume,
		AgentImage: state.runtimeProfile.Contract().Image,
	}, state.settings)
	if err != nil {
		return err
	}
	defer probe.Close()
	return probe.Inspect(ctx)
}

func (state *productionState) checkRuntimeImage(ctx context.Context) error {
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	availability, err := composition.NewImageAvailability(state.settings)
	if err != nil {
		return err
	}
	defer availability.Close()
	for _, profile := range state.runtimeProfiles {
		contract := profile.Contract()
		if err := availability.Available(ctx, contract.Image, contract.Platform); err != nil {
			return err
		}
		if err := composition.CheckRepositoryEnvironment(ctx, profile, state.settings); err != nil {
			return err
		}
	}
	return nil
}

func (state *productionState) checkAgentRuntime(ctx context.Context) error {
	if len(state.runtimeProfiles) == 0 {
		return errors.New("Runtime Profile contract check did not pass")
	}
	endpoint, err := url.Parse(state.settings.MCPEndpointURL)
	if err != nil || endpoint.Hostname() == "" {
		return errors.New("configured MCP endpoint does not have a host")
	}
	options := piProbeOptions{
		Settings:    state.settings,
		Network:     state.settings.DockerAgentNetwork,
		MCPHost:     endpoint.Hostname(),
		MemoryBytes: state.settings.AgentTurnMemoryBytes,
	}
	for _, profile := range state.runtimeProfiles {
		if err := checkPi(ctx, profile, options); err != nil {
			return err
		}
	}
	return nil
}

func (state *productionState) checkProtectedRuntimeProfiles(ctx context.Context) error {
	if state.database == nil {
		return errors.New("PostgreSQL connectivity check did not pass")
	}
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	availability, err := composition.NewImageAvailability(state.settings)
	if err != nil {
		return err
	}
	defer availability.Close()
	checker, err := runtimeprofile.NewAvailabilityChecker(state.database, runtimeprofile.Resolver{Catalog: state.registry.Catalog, Source: state.database}, availability)
	if err != nil {
		return err
	}
	return checker.Check(ctx)
}

func (state *productionState) checkCompatibilityArtifact(context.Context) error {
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	return validateCompatibilityFile(state.settings.RuntimeProfileCompatibilityResultsFile, composition.StateBinding(state.settings).Family, state.runtimeProfiles...)
}

func (state *productionState) checkPendingRuntimeCompatibility(ctx context.Context) error {
	if state.database == nil {
		return errors.New("PostgreSQL connectivity check did not pass")
	}
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	for _, profile := range state.runtimeProfiles {
		if err := state.database.CheckPendingRuntimeProfileCompatibility(ctx, profile); err != nil {
			return err
		}
	}
	return nil
}

func (state *productionState) githubAPI() (*githubapi.APIClient, error) {
	if state.api != nil {
		return state.api, nil
	}
	api, err := githubapi.NewAPIClient(&http.Client{Timeout: state.settings.ReadinessTimeout}, state.settings.GitHubAPIURL)
	if err != nil {
		return nil, err
	}
	state.api = api
	return api, nil
}

func (state *productionState) checkDeveloperInstallation(ctx context.Context) error {
	if err := state.checkAppConfiguration(ctx, state.developer, state.settings.GitHubDeveloperAppID,
		githubapi.DeveloperAppPermissions(), []string{"issues", "pull_request", "pull_request_review", "repository"}); err != nil {
		return err
	}
	if err := state.checkDeveloperWebhook(ctx); err != nil {
		return err
	}
	credential, err := state.repositoryCredential(ctx, state.developer, githubapi.DeveloperAppPermissions())
	if err == nil {
		state.developerToken = credential
	}
	return err
}

func (state *productionState) checkDeveloperWebhook(ctx context.Context) error {
	api, err := state.githubAPI()
	if err != nil {
		return err
	}
	appJWT, err := state.developer.AppJWT(ctx)
	if err != nil {
		return err
	}
	configuration, err := api.GetAppWebhookConfig(ctx, appJWT)
	if err != nil {
		return err
	}
	if err := validateDeveloperWebhookURL(configuration.URL); err != nil {
		return err
	}
	if configuration.Secret == "" {
		return errors.New("Developer GitHub App webhook secret is not configured")
	}
	if !strings.EqualFold(configuration.ContentType, "json") {
		return errors.New("Developer GitHub App webhook content type must be json")
	}
	if !configuration.VerifiesTLS() {
		return errors.New("Developer GitHub App webhook must verify TLS certificates")
	}
	return nil
}

func validateDeveloperWebhookURL(rawURL string) error {
	webhookURL, err := url.Parse(rawURL)
	if err != nil || webhookURL.Scheme != "https" || webhookURL.Host == "" || webhookURL.User != nil ||
		webhookURL.Path != "/webhooks/github" || webhookURL.RawQuery != "" || webhookURL.Fragment != "" {
		return errors.New("Developer GitHub App webhook must use a public HTTPS /webhooks/github URL")
	}
	host := webhookURL.Hostname()
	address := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") ||
		strings.HasSuffix(strings.ToLower(host), ".local") || address != nil &&
		(address.IsLoopback() || address.IsPrivate() || address.IsUnspecified() || address.IsLinkLocalUnicast()) {
		return errors.New("Developer GitHub App webhook must use a public HTTPS /webhooks/github URL")
	}
	return nil
}

func (state *productionState) checkReviewerInstallation(ctx context.Context) error {
	if err := state.checkAppConfiguration(ctx, state.reviewer, state.settings.GitHubReviewerAppID,
		githubapi.ReviewerAppPermissions(), []string{}); err != nil {
		return err
	}
	if err := state.checkReviewerWebhook(ctx); err != nil {
		return err
	}
	credential, err := state.repositoryCredential(ctx, state.reviewer, githubapi.ReviewerAppPermissions())
	if err == nil {
		state.reviewerToken = credential
	}
	return err
}

func (state *productionState) checkReviewerWebhook(ctx context.Context) error {
	api, err := state.githubAPI()
	if err != nil {
		return err
	}
	appJWT, err := state.reviewer.AppJWT(ctx)
	if err != nil {
		return err
	}
	configuration, err := api.GetAppWebhookConfig(ctx, appJWT)
	return validateReviewerWebhookConfiguration(configuration, err)
}

func validateReviewerWebhookConfiguration(configuration githubapi.AppWebhookConfig, requestErr error) error {
	if requestErr != nil {
		var apiError *githubapi.APIError
		if errors.As(requestErr, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return nil
		}
		return requestErr
	}
	if configuration.URL != "" || configuration.Secret != "" {
		return errors.New("Reviewer GitHub App webhook must be disabled")
	}
	return nil
}

func (state *productionState) checkAppConfiguration(ctx context.Context, signer *githubapi.AppJWTSigner, appID int64, permissions githubapi.InstallationPermissions, events []string) error {
	if signer == nil {
		return errors.New("GitHub App key check did not pass")
	}
	api, err := state.githubAPI()
	if err != nil {
		return err
	}
	appJWT, err := signer.AppJWT(ctx)
	if err != nil {
		return err
	}
	app, err := api.GetAuthenticatedApp(ctx, appJWT)
	if err != nil {
		return err
	}
	if app.ID != appID {
		return fmt.Errorf("authenticated GitHub App ID is %d, want %d", app.ID, appID)
	}
	if !sameStringMap(app.Permissions, permissions) {
		return errors.New("GitHub App permissions do not match the required policy")
	}
	if !sameStringSet(app.Events, events) {
		return errors.New("GitHub App event subscriptions do not match the required policy")
	}
	return nil
}

func (state *productionState) repositoryCredential(ctx context.Context, signer *githubapi.AppJWTSigner, permissions githubapi.InstallationPermissions) (string, error) {
	if signer == nil {
		return "", errors.New("GitHub App key check did not pass")
	}
	api, err := state.githubAPI()
	if err != nil {
		return "", err
	}
	provider, err := githubapi.NewRepositoryInstallationCredentialProvider(signer, api, permissions, nil)
	if err != nil {
		return "", err
	}
	return provider.RepositoryCredential(ctx, state.repo.Owner, state.repo.Name)
}

func (state *productionState) checkAgentProfiles(ctx context.Context) error {
	if state.developerToken == "" {
		return errors.New("Developer GitHub installation check did not pass")
	}
	api, err := state.githubAPI()
	if err != nil {
		return err
	}
	snapshot, err := agentprofile.NewLoader(api, role.BuiltinPolicyCatalog(), state.settings.AgentPathEnvironmentAllowlist...).Load(ctx, state.developerToken, state.repo.Owner, state.repo.Name)
	if err != nil {
		return err
	}
	if _, err := reviewpolicy.LoadAtCommit(ctx, api, state.developerToken, state.repo.Owner, state.repo.Name, snapshot.CommitSHA()); err != nil {
		return fmt.Errorf("review scheduling policy: %w", err)
	}
	selection, err := snapshot.Select(agentprofile.SingletonSelector{})
	if err != nil {
		return err
	}
	state.profiles = snapshot
	state.profileSelection = selection
	for _, roleID := range role.BuiltinPolicyCatalog().Roles() {
		name, _ := selection.Profile(roleID)
		p, _ := snapshot.Profile(name)
		selected, err := state.repositoryRuntime(p)
		if err != nil {
			return err
		}
		if err := composition.CheckRepositoryEnvironment(ctx, selected, state.settings); err != nil {
			return err
		}
	}
	state.profilesLoaded = true
	return nil
}

func (state *productionState) repositoryRuntime(p agentprofile.Profile) (runtimeprofile.Profile, error) {
	name, version, _ := strings.Cut(p.Runtime(), "/")
	selected, err := state.registry.Resolve(name, version)
	if err != nil {
		return runtimeprofile.Profile{}, err
	}
	if definition := state.profiles.Environment(); definition != nil {
		os, arch, _ := strings.Cut(definition.Platform, "/")
		var requirements []string
		for _, requirement := range definition.Requirements {
			requirements = append(requirements, string(requirement))
		}
		return selected.WithEnvironment(definition.Image, runtimeprofile.Platform{OS: os, Arch: arch}, requirements)
	}
	return selected, nil
}

func (state *productionState) checkEffectiveProfiles(context.Context) error {
	if !state.profilesLoaded {
		return errors.New("Agent Profile check did not pass")
	}
	if !state.operatorInstructionsLoaded {
		return errors.New("operator agent instructions check did not pass")
	}
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		return err
	}
	var credentials []byte
	if state.settings.OpenAIAuthMode == "subscription" {
		credentials, err = providerauth.SubscriptionBundle(state.settings.ProviderCredentialsFile)
	} else {
		credentials, err = os.ReadFile(state.settings.ProviderCredentialsFile)
	}
	if err != nil {
		return errors.New("provider credentials are unavailable")
	}
	defer zero(credentials)
	for _, roleID := range role.BuiltinPolicyCatalog().Roles() {
		name, selected := state.profileSelection.Profile(roleID)
		profile, ok := state.profiles.Profile(name)
		if !selected || !ok {
			return fmt.Errorf("%s: Agent Profile is unavailable", roleID)
		}
		if err := validateEffectiveProfile(state.registry, roleID, profile, state.operatorInstructions, definition, credentials); err != nil {
			return fmt.Errorf("%s: %w", roleID, err)
		}
	}
	state.effectiveProfilesLoaded = true
	return nil
}

func (state *productionState) checkEffectiveRuntimes(ctx context.Context) error {
	if !state.effectiveProfilesLoaded {
		return errors.New("effective Agent Profile check did not pass")
	}
	endpoint, err := url.Parse(state.settings.MCPEndpointURL)
	if err != nil || endpoint.Hostname() == "" {
		return errors.New("configured MCP endpoint does not have a host")
	}
	for _, roleID := range role.BuiltinPolicyCatalog().Roles() {
		name, _ := state.profileSelection.Profile(roleID)
		p, _ := state.profiles.Profile(name)
		selected, err := state.repositoryRuntime(p)
		if err != nil {
			return err
		}
		if err := checkPi(ctx, selected, piProbeOptions{Settings: state.settings, Network: state.settings.DockerAgentNetwork, MCPHost: endpoint.Hostname(), MemoryBytes: state.settings.AgentTurnMemoryBytes}, agent.ModelSelection{Model: p.Model(), Thinking: p.Thinking()}); err != nil {
			return fmt.Errorf("%s: %w", roleID, err)
		}
	}
	return nil
}

func (state *productionState) checkMCPEndpoint(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, state.settings.MCPEndpointURL, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: state.settings.ReadinessTimeout}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("unauthenticated MCP request returned HTTP %d, want 401", response.StatusCode)
	}
	return nil
}

func readSigner(appID int64, path string) (*githubapi.AppJWTSigner, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer zero(contents)
	return githubapi.NewAppJWTSigner(appID, contents, nil)
}

func validateNonemptyFile(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	defer zero(contents)
	if strings.TrimSpace(string(contents)) == "" {
		return errors.New("file is empty")
	}
	return nil
}

func validateJSONObjectFile(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	defer zero(contents)
	var object map[string]json.RawMessage
	defer func() {
		for name, value := range object {
			zero(value)
			delete(object, name)
		}
	}()
	if json.Unmarshal(contents, &object) != nil || len(object) == 0 {
		return errors.New("file must contain a nonempty JSON object")
	}
	return nil
}

func validateCompatibilityFile(path, backend string, targets ...runtimeprofile.Profile) error {
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	results, err := runtimeprofile.DecodeCompatibilityResultsFile(file)
	if err != nil {
		return err
	}
	return results.ValidateDeployment(backend, 1, targets...)
}

func validateEffectiveProfile(registry runtimeprofile.Registry, roleID workflow.Role, profile agentprofile.Profile, operator agentinstructions.Operator, definition workflow.Definition, credentials ...[]byte) error {
	name, version, found := strings.Cut(profile.Runtime(), "/")
	if !found || name == "" || version == "" || strings.Contains(version, "/") {
		return errors.New("invalid Runtime Profile reference")
	}
	resolved, err := registry.Resolve(name, version)
	if err != nil {
		return err
	}
	contract := resolved.Contract()
	if contract.Name != name || contract.Version != version {
		return errors.New("resolved Runtime Profile does not match the requested reference")
	}
	policy, _ := role.BuiltinPolicyCatalog().Lookup(roleID)
	for _, stageID := range definition.StageIDs() {
		stage, _ := definition.Stage(stageID)
		if stage.Role != roleID {
			continue
		}
		for _, purpose := range stage.AcceptedPurposes {
			var auth []byte
			if len(credentials) > 0 {
				auth = credentials[0]
			}
			err := composition.ValidatePiProfile(profile, policy, operator, stage, purpose, agent.MCPEndpoint{Name: "omnigrex", URL: "http://diagnostic.invalid/mcp", Headers: []agent.Header{{Name: "Authorization", Value: "Bearer fixture-only"}}}, auth)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", stageID, purpose, err)
			}
		}
	}
	return nil
}

func zero(contents []byte) {
	for index := range contents {
		contents[index] = 0
	}
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	wanted := make(map[string]struct{}, len(right))
	for _, value := range right {
		wanted[value] = struct{}{}
	}
	if len(wanted) != len(right) {
		return false
	}
	for _, value := range left {
		if _, ok := wanted[value]; !ok {
			return false
		}
		delete(wanted, value)
	}
	return len(wanted) == 0
}
