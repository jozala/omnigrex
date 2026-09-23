package github_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

type credentialJWTProvider struct {
	token string
	calls int
}

func (provider *credentialJWTProvider) AppJWT(context.Context) (string, error) {
	provider.calls++
	return provider.token, nil
}

type repositoryInstallationAPI struct {
	resolveJWT        string
	resolveOwner      string
	resolveRepository string
	installationID    int64
	resolveErr        error
	tokenJWT          string
	tokenID           int64
	tokenPermissions  githubapi.InstallationPermissions
	tokenErr          error
}

func (api *repositoryInstallationAPI) ResolveRepositoryInstallation(_ context.Context, jwt, owner, repository string) (int64, error) {
	api.resolveJWT, api.resolveOwner, api.resolveRepository = jwt, owner, repository
	return api.installationID, api.resolveErr
}

func (api *repositoryInstallationAPI) CreateInstallationToken(_ context.Context, jwt string, installationID int64) (githubapi.InstallationToken, error) {
	api.tokenJWT, api.tokenID = jwt, installationID
	if api.tokenErr != nil {
		return githubapi.InstallationToken{}, api.tokenErr
	}
	return githubapi.InstallationToken{
		Token: "installation-token", ExpiresAt: time.Now().Add(time.Hour), Permissions: api.tokenPermissions,
	}, nil
}

func TestRepositoryInstallationCredentialProviderRequiresExactRolePermissions(t *testing.T) {
	tests := []struct {
		name        string
		required    githubapi.InstallationPermissions
		permissions githubapi.InstallationPermissions
		wantError   bool
	}{
		{
			name:        "Developer exact permissions",
			required:    githubapi.DeveloperAppPermissions(),
			permissions: githubapi.DeveloperAppPermissions(),
		},
		{
			name:        "Reviewer exact permissions",
			required:    githubapi.ReviewerAppPermissions(),
			permissions: githubapi.ReviewerAppPermissions(),
		},
		{
			name:     "Reviewer missing Pull Request write",
			required: githubapi.ReviewerAppPermissions(),
			permissions: githubapi.InstallationPermissions{
				"metadata": "read", "contents": "read", "pull_requests": "read",
			},
			wantError: true,
		},
		{
			name:     "Developer has an undeclared administration permission",
			required: githubapi.DeveloperAppPermissions(),
			permissions: func() githubapi.InstallationPermissions {
				permissions := githubapi.DeveloperAppPermissions()
				permissions["administration"] = "write"
				return permissions
			}(),
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jwt := &credentialJWTProvider{token: "app-jwt"}
			api := &repositoryInstallationAPI{installationID: 42, tokenPermissions: test.permissions}
			provider, err := githubapi.NewRepositoryInstallationCredentialProvider(jwt, api, test.required, nil)
			if err != nil {
				t.Fatal(err)
			}

			credential, err := provider.RepositoryCredential(context.Background(), "acme", "widgets")
			if test.wantError {
				if credential != "" || !githubapi.ExtractSafeErrorMetadata(err).Permanent || !strings.Contains(err.Error(), githubapi.ErrInstallationPermissions.Error()) {
					t.Fatalf("RepositoryCredential() = (%q, %v), want permanent permission configuration error", credential, err)
				}
				return
			}
			if err != nil || credential != "installation-token" {
				t.Fatalf("RepositoryCredential() = (%q, %v), want accepted role credential", credential, err)
			}
		})
	}
}

func TestRepositoryInstallationCredentialProviderDoesNotCacheRejectedPermissions(t *testing.T) {
	jwt := &credentialJWTProvider{token: "app-jwt"}
	api := &repositoryInstallationAPI{
		installationID: 42,
		tokenPermissions: githubapi.InstallationPermissions{
			"metadata": "read", "contents": "read", "pull_requests": "read",
		},
	}
	provider, err := githubapi.NewRepositoryInstallationCredentialProvider(jwt, api, githubapi.ReviewerAppPermissions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RepositoryCredential(context.Background(), "acme", "widgets"); err == nil || !githubapi.ExtractSafeErrorMetadata(err).Permanent {
		t.Fatalf("first RepositoryCredential() error = %v, want permission rejection", err)
	}

	api.tokenPermissions = githubapi.ReviewerAppPermissions()
	credential, err := provider.RepositoryCredential(context.Background(), "acme", "widgets")
	if err != nil || credential != "installation-token" {
		t.Fatalf("second RepositoryCredential() = (%q, %v), want recovered credential", credential, err)
	}
	if jwt.calls != 4 {
		t.Fatalf("AppJWT() calls = %d, want two installation resolutions and two uncached token requests", jwt.calls)
	}
}

type rotatingCredentialJWTProvider struct {
	tokens []string
	calls  int
}

type secretBearingError struct {
	message string
}

func (err *secretBearingError) Error() string { return err.message }

func (provider *rotatingCredentialJWTProvider) AppJWT(context.Context) (string, error) {
	token := provider.tokens[provider.calls]
	provider.calls++
	return token, nil
}

func TestRepositoryInstallationCredentialProviderResolvesInstallationAndUsesTokenCache(t *testing.T) {
	jwt := &credentialJWTProvider{token: "app-jwt"}
	api := &repositoryInstallationAPI{installationID: 42}
	provider, err := githubapi.NewRepositoryInstallationCredentialProvider(jwt, api, nil, nil)
	if err != nil {
		t.Fatalf("NewRepositoryInstallationCredentialProvider() error = %v", err)
	}

	for range 2 {
		credential, err := provider.RepositoryCredential(context.Background(), "acme", "widgets")
		if err != nil || credential != "installation-token" {
			t.Fatalf("RepositoryCredential() = (%q, %v), want installation-token", credential, err)
		}
	}
	if api.resolveJWT != "app-jwt" || api.resolveOwner != "acme" || api.resolveRepository != "widgets" {
		t.Errorf("ResolveRepositoryInstallation() = (%q, %q, %q)", api.resolveJWT, api.resolveOwner, api.resolveRepository)
	}
	if api.tokenJWT != "app-jwt" || api.tokenID != 42 {
		t.Errorf("CreateInstallationToken() = (%q, %d), want app-jwt and 42", api.tokenJWT, api.tokenID)
	}
	if jwt.calls != 3 {
		t.Errorf("AppJWT() calls = %d, want two resolutions and one cached token creation", jwt.calls)
	}
}

func TestRepositoryInstallationCredentialProviderRedactsAppJWTFromErrors(t *testing.T) {
	sourceErr := &secretBearingError{message: "repository rejected app-jwt"}
	jwt := &credentialJWTProvider{token: "app-jwt"}
	api := &repositoryInstallationAPI{resolveErr: sourceErr}
	provider, err := githubapi.NewRepositoryInstallationCredentialProvider(jwt, api, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = provider.RepositoryCredential(context.Background(), "acme", "widgets")
	if strings.Contains(err.Error(), "app-jwt") {
		t.Fatalf("RepositoryCredential() leaked App JWT: %v", err)
	}
	assertSecretSourceUnreachable(t, err, sourceErr)
}

func TestRepositoryInstallationCredentialProviderRedactsTokenCreationJWTFromErrors(t *testing.T) {
	sourceErr := &secretBearingError{message: "token endpoint rejected token-jwt"}
	jwt := &rotatingCredentialJWTProvider{tokens: []string{"resolve-jwt", "token-jwt"}}
	api := &repositoryInstallationAPI{installationID: 42, tokenErr: sourceErr}
	provider, err := githubapi.NewRepositoryInstallationCredentialProvider(jwt, api, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = provider.RepositoryCredential(context.Background(), "acme", "widgets")
	if strings.Contains(err.Error(), "token-jwt") || strings.Contains(err.Error(), "resolve-jwt") {
		t.Fatalf("RepositoryCredential() leaked App JWT: %v", err)
	}
	assertSecretSourceUnreachable(t, err, sourceErr)
}

func TestRepositoryInstallationCredentialProviderPreservesOnlySafeErrorMetadata(t *testing.T) {
	resetAt := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	tests := []struct {
		name string
		err  error
		want githubapi.SafeErrorMetadata
	}{
		{
			name: "permanent",
			err:  &githubapi.ConfigurationError{Cause: errors.New("invalid app-jwt")},
			want: githubapi.SafeErrorMetadata{Permanent: true},
		},
		{
			name: "transient",
			err:  &githubapi.TransientError{Cause: errors.New("unavailable app-jwt")},
			want: githubapi.SafeErrorMetadata{Transient: true},
		},
		{
			name: "API client error",
			err:  &githubapi.APIError{StatusCode: 404, Message: "missing app-jwt"},
			want: githubapi.SafeErrorMetadata{APIClientError: true},
		},
		{
			name: "API request timeout",
			err:  &githubapi.APIError{StatusCode: 408, Message: "timeout app-jwt"},
			want: githubapi.SafeErrorMetadata{APIClientError: true, APIRetryable: true},
		},
		{
			name: "API too many requests",
			err:  &githubapi.APIError{StatusCode: 429, Message: "limited app-jwt"},
			want: githubapi.SafeErrorMetadata{APIClientError: true, APIRetryable: true},
		},
		{
			name: "rate limit timing",
			err: &githubapi.RateLimitError{
				APIError:   &githubapi.APIError{StatusCode: 429, Message: "limited app-jwt"},
				RetryAfter: 7 * time.Second,
				ResetAt:    resetAt,
			},
			want: githubapi.SafeErrorMetadata{
				Transient: true, APIClientError: true, APIRetryable: true,
				RetryAfter: 7 * time.Second, ResetAt: resetAt,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := githubapi.NewRepositoryInstallationCredentialProvider(
				&credentialJWTProvider{token: "app-jwt"},
				&repositoryInstallationAPI{resolveErr: test.err},
				nil,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			_, err = provider.RepositoryCredential(context.Background(), "acme", "widgets")
			if got := githubapi.ExtractSafeErrorMetadata(err); got != test.want {
				t.Fatalf("safe metadata = %#v, want %#v", got, test.want)
			}
			if errors.Is(err, test.err) {
				t.Fatalf("sanitized error retained source through errors.Is: %v", err)
			}
			var apiError *githubapi.APIError
			if errors.As(err, &apiError) {
				t.Fatalf("sanitized error exposed APIError: %#v", apiError)
			}
			var rateLimit *githubapi.RateLimitError
			if errors.As(err, &rateLimit) {
				t.Fatalf("sanitized error exposed RateLimitError: %#v", rateLimit)
			}
		})
	}
}

func assertSecretSourceUnreachable(t *testing.T, err error, source *secretBearingError) {
	t.Helper()
	if errors.Is(err, source) {
		t.Fatalf("sanitized error retained source through errors.Is: %v", err)
	}
	var recovered *secretBearingError
	if errors.As(err, &recovered) {
		t.Fatalf("sanitized error retained source through errors.As: %#v", recovered)
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if current == source {
			t.Fatalf("sanitized error retained source in Unwrap chain: %v", current)
		}
	}
}

func TestNewRepositoryInstallationCredentialProviderRejectsNilDependencies(t *testing.T) {
	jwt := &credentialJWTProvider{token: "app-jwt"}
	api := &repositoryInstallationAPI{installationID: 42}
	if _, err := githubapi.NewRepositoryInstallationCredentialProvider(nil, api, nil, nil); err == nil {
		t.Error("nil AppJWTProvider error = nil")
	}
	if _, err := githubapi.NewRepositoryInstallationCredentialProvider(jwt, nil, nil, nil); err == nil {
		t.Error("nil repository API error = nil")
	}
}
