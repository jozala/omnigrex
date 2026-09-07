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

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/config"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type productionState struct {
	settings config.Config
	repo     Repository

	runtimeProfile runtimeprofile.Profile
	registry       runtimeprofile.Registry
	database       *store.Store
	api            *githubapi.APIClient
	developer      *githubapi.AppJWTSigner
	reviewer       *githubapi.AppJWTSigner
	developerToken string
	reviewerToken  string
	profiles       agentprofile.Snapshot
	profilesLoaded bool
}

// RunProduction runs read-only deployment and repository checks in deterministic order.
// GitHub installation-token creation is the only non-GET external request; it does not mutate repository content.
func RunProduction(ctx context.Context, settings config.Config, repository Repository) ([]Result, error) {
	state := &productionState{settings: settings, repo: repository}
	defer state.close()
	checks := []Check{
		{Name: "runtime-profile-contract", Run: state.checkRuntimeProfile},
		{Name: "runtime-profile-compatibility-artifact", Run: state.checkCompatibilityArtifact},
		{Name: "developer-provider-credentials", Run: func(context.Context) error { return validateJSONObjectFile(settings.DeveloperProviderCredentialsFile) }},
		{Name: "reviewer-provider-credentials", Run: func(context.Context) error { return validateJSONObjectFile(settings.ReviewerProviderCredentialsFile) }},
		{Name: "github-webhook-secret", Run: func(context.Context) error { return validateNonemptyFile(settings.GitHubWebhookSecretFile) }},
		{Name: "developer-github-app-key", Run: state.checkDeveloperKey},
		{Name: "reviewer-github-app-key", Run: state.checkReviewerKey},
		{Name: "github-app-role-separation", Run: state.checkRoleSeparation},
		{Name: "postgres-connectivity", Run: state.checkDatabase},
		{Name: "postgres-schema", Run: state.checkDatabaseSchema},
		{Name: "docker-resources", Run: state.checkDockerResources},
		{Name: "runtime-profile-image", Run: state.checkRuntimeImage},
		{Name: "acp-runtime", Run: state.checkACPRuntime},
		{Name: "protected-runtime-profiles", Run: state.checkProtectedRuntimeProfiles},
		{Name: "pending-runtime-profile-compatibility", Run: state.checkPendingRuntimeCompatibility},
		{Name: "developer-github-installation", Run: state.checkDeveloperInstallation},
		{Name: "reviewer-github-installation", Run: state.checkReviewerInstallation},
		{Name: "agent-profiles", Run: state.checkAgentProfiles},
		{Name: "reviewer-repository-read", Run: state.checkReviewerRepositoryRead},
		{Name: "effective-agent-profiles", Run: state.checkEffectiveProfiles},
		{Name: "mcp-endpoint", Run: state.checkMCPEndpoint},
	}
	runner, err := NewRunner(settings.ReadinessTimeout, checks)
	if err != nil {
		return nil, err
	}
	return runner.Run(ctx), nil
}

func (state *productionState) close() {
	if state.database != nil {
		state.database.Close()
	}
	state.developerToken = ""
	state.reviewerToken = ""
}

func (state *productionState) checkRuntimeProfile(context.Context) error {
	profile, err := runtimeprofile.NewOpenCodeV1(state.settings.OpenCodeACPV1Image, state.settings.OpenCodeACPV1Platform)
	if err != nil {
		return err
	}
	registry, err := runtimeprofile.NewRegistry(profile)
	if err != nil {
		return err
	}
	state.runtimeProfile = profile
	state.registry = registry
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
	probe, err := dockerruntime.NewReadinessProbe(dockerruntime.ReadinessProbeOptions{
		AgentNetwork: state.settings.DockerAgentNetwork, WorkspaceVolume: state.settings.WorkspaceVolume,
		RuntimeStateVolume: state.settings.RuntimeStateVolume, MiseVolume: state.settings.MiseVolume,
		AgentImage: state.settings.AgentImageReference,
	})
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
	availability, err := dockerruntime.NewExactImageAvailability()
	if err != nil {
		return err
	}
	defer availability.Close()
	contract := state.runtimeProfile.Contract()
	return availability.Available(ctx, contract.Image, contract.Platform)
}

func (state *productionState) checkACPRuntime(ctx context.Context) error {
	endpoint, err := url.Parse(state.settings.MCPEndpointURL)
	if err != nil || endpoint.Hostname() == "" {
		return errors.New("configured MCP endpoint does not have a host")
	}
	return checkACP(ctx, state.runtimeProfile, acpProbeOptions{
		Network: state.settings.DockerAgentNetwork,
		MCPHost: endpoint.Hostname(),
	})
}

func (state *productionState) checkProtectedRuntimeProfiles(ctx context.Context) error {
	if state.database == nil {
		return errors.New("PostgreSQL connectivity check did not pass")
	}
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	availability, err := dockerruntime.NewExactImageAvailability()
	if err != nil {
		return err
	}
	defer availability.Close()
	checker, err := runtimeprofile.NewAvailabilityChecker(state.database, state.registry.Catalog, availability)
	if err != nil {
		return err
	}
	return checker.Check(ctx)
}

func (state *productionState) checkCompatibilityArtifact(context.Context) error {
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	return validateCompatibilityFile(state.settings.RuntimeProfileCompatibilityResultsFile, state.runtimeProfile)
}

func (state *productionState) checkPendingRuntimeCompatibility(ctx context.Context) error {
	if state.database == nil {
		return errors.New("PostgreSQL connectivity check did not pass")
	}
	if state.runtimeProfile.Contract().Name == "" {
		return errors.New("Runtime Profile contract check did not pass")
	}
	return state.database.CheckPendingRuntimeProfileCompatibility(ctx, state.runtimeProfile)
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
		githubapi.DeveloperAppPermissions(), []string{"issues", "pull_request", "pull_request_review"}); err != nil {
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
	snapshot, err := agentprofile.NewLoader(api).Load(ctx, state.developerToken, state.repo.Owner, state.repo.Name)
	if err != nil {
		return err
	}
	state.profiles = snapshot
	state.profilesLoaded = true
	return nil
}

func (state *productionState) checkReviewerRepositoryRead(ctx context.Context) error {
	if state.reviewerToken == "" || !state.profilesLoaded {
		return errors.New("Reviewer installation and Agent Profile checks must pass first")
	}
	api, err := state.githubAPI()
	if err != nil {
		return err
	}
	want := state.profiles.Developer()
	content, err := api.FetchRepositoryFile(ctx, state.reviewerToken, state.repo.Owner, state.repo.Name, want.Path(), state.profiles.CommitSHA())
	if err != nil {
		return err
	}
	got, err := agentprofile.Parse(agentprofile.Developer, content)
	if err != nil {
		return err
	}
	if got.ContentSHA256() != want.ContentSHA256() {
		return errors.New("Reviewer read a different immutable Agent Profile")
	}
	return nil
}

func (state *productionState) checkEffectiveProfiles(context.Context) error {
	if !state.profilesLoaded {
		return errors.New("Agent Profile check did not pass")
	}
	if err := validateEffectiveProfile(state.registry, workflow.RoleDeveloper, state.profiles.Developer()); err != nil {
		return fmt.Errorf("Developer: %w", err)
	}
	if err := validateEffectiveProfile(state.registry, workflow.RoleReviewer, state.profiles.Reviewer()); err != nil {
		return fmt.Errorf("Reviewer: %w", err)
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

func validateCompatibilityFile(path string, target runtimeprofile.Profile) error {
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
	return results.ValidateTarget(target)
}

func validateEffectiveProfile(registry runtimeprofile.Registry, role workflow.Role, profile agentprofile.Profile) error {
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
	permissions := make(opencode.PermissionPolicy, len(profile.Permissions()))
	for name, action := range profile.Permissions() {
		permissions[name] = opencode.Permission(action)
	}
	capabilities, err := mcp.CapabilitiesForRole(role)
	if err != nil {
		return err
	}
	runtimeTools := make([]string, len(capabilities))
	for index, capability := range capabilities {
		runtimeTools[index] = mcp.ServerName + "_" + capability
	}
	openCodeRole := opencode.RoleDeveloper
	if role == workflow.RoleReviewer {
		openCodeRole = opencode.RoleReviewer
	}
	_, err = opencode.Render(openCodeRole, opencode.Profile{
		Instructions: profile.Instructions(), Model: profile.Model(), Variant: profile.Variant(), Steps: uint(profile.Steps()),
		Permissions: permissions, RuntimeTools: runtimeTools,
	})
	return err
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
