package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrInstallationPermissions = errors.New("GitHub App installation permissions do not match the required policy")

// DeveloperAppPermissions returns the exact least-privilege repository permissions required by the Developer App.
func DeveloperAppPermissions() InstallationPermissions {
	return InstallationPermissions{
		"metadata":      "read",
		"checks":        "read",
		"contents":      "write",
		"issues":        "write",
		"pull_requests": "write",
		"workflows":     "write",
	}
}

// ReviewerAppPermissions returns the exact least-privilege repository permissions required by the Reviewer App.
func ReviewerAppPermissions() InstallationPermissions {
	return InstallationPermissions{
		"metadata":      "read",
		"contents":      "read",
		"pull_requests": "write",
	}
}

// RepositoryInstallationAPI is the narrow GitHub API surface needed to mint repository credentials.
type RepositoryInstallationAPI interface {
	ResolveRepositoryInstallation(context.Context, string, string, string) (int64, error)
	CreateInstallationToken(context.Context, string, int64) (InstallationToken, error)
}

// RepositoryInstallationCredentialProvider resolves a repository's App installation and returns a cached token.
type RepositoryInstallationCredentialProvider struct {
	appJWT AppJWTProvider
	api    RepositoryInstallationAPI
	tokens *InstallationTokenCache
}

// NewRepositoryInstallationCredentialProvider creates a credential provider for one GitHub App identity.
func NewRepositoryInstallationCredentialProvider(appJWT AppJWTProvider, api RepositoryInstallationAPI, requiredPermissions InstallationPermissions, clock Clock) (*RepositoryInstallationCredentialProvider, error) {
	if appJWT == nil {
		return nil, errors.New("repository installation credential App JWT provider is nil")
	}
	if api == nil {
		return nil, errors.New("repository installation credential API is nil")
	}
	return &RepositoryInstallationCredentialProvider{
		appJWT: appJWT,
		api:    api,
		tokens: NewInstallationTokenCache(appJWT, credentialSafeTokenRequester{
			api: api, requiredPermissions: cloneInstallationPermissions(requiredPermissions),
		}, clock),
	}, nil
}

// RepositoryCredential returns a short-lived installation token for the named repository.
func (provider *RepositoryInstallationCredentialProvider) RepositoryCredential(ctx context.Context, owner, repository string) (credential string, err error) {
	if provider == nil || provider.appJWT == nil || provider.api == nil || provider.tokens == nil {
		return "", errors.New("repository installation credential provider is not configured")
	}
	appJWT, err := provider.appJWT.AppJWT(ctx)
	if err != nil {
		return "", fmt.Errorf("create GitHub App JWT for repository installation: %w", err)
	}
	defer func() {
		if err != nil {
			err = redactSecret(err, appJWT)
		}
	}()
	installationID, err := provider.api.ResolveRepositoryInstallation(ctx, appJWT, owner, repository)
	if err != nil {
		return "", fmt.Errorf("resolve GitHub repository installation: %w", err)
	}
	credential, err = provider.tokens.Token(ctx, installationID)
	if err != nil {
		return "", fmt.Errorf("create GitHub repository installation credential: %w", err)
	}
	return credential, nil
}

type secretSafeError struct {
	message  string
	metadata SafeErrorMetadata
}

func (err secretSafeError) Error() string { return err.message }
func (err secretSafeError) SafeErrorMetadata() SafeErrorMetadata {
	return err.metadata
}

func redactSecret(err error, secret string) error {
	if secret == "" {
		return err
	}
	message := err.Error()
	message = strings.ReplaceAll(message, secret, "[REDACTED]")
	return secretSafeError{message: message, metadata: ExtractSafeErrorMetadata(err)}
}

type credentialSafeTokenRequester struct {
	api                 RepositoryInstallationAPI
	requiredPermissions InstallationPermissions
}

func (requester credentialSafeTokenRequester) CreateInstallationToken(ctx context.Context, appJWT string, installationID int64) (InstallationToken, error) {
	token, err := requester.api.CreateInstallationToken(ctx, appJWT, installationID)
	if err != nil {
		return InstallationToken{}, redactSecret(err, appJWT)
	}
	if !hasExactInstallationPermissions(token.Permissions, requester.requiredPermissions) {
		return InstallationToken{}, &ConfigurationError{Cause: ErrInstallationPermissions}
	}
	return token, nil
}

func hasExactInstallationPermissions(actual, required InstallationPermissions) bool {
	if len(required) == 0 {
		return true
	}
	if len(actual) != len(required) {
		return false
	}
	for permission, level := range required {
		if actual[permission] != level {
			return false
		}
	}
	return true
}

func cloneInstallationPermissions(permissions InstallationPermissions) InstallationPermissions {
	if permissions == nil {
		return nil
	}
	cloned := make(InstallationPermissions, len(permissions))
	for permission, level := range permissions {
		cloned[permission] = level
	}
	return cloned
}
