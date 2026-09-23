//go:build live

package github_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestLiveReviewerAppSubmitsApproveAndRequestChanges(t *testing.T) {
	required := []string{
		"OMNIGREX_LIVE_GITHUB_OWNER",
		"OMNIGREX_LIVE_GITHUB_REPOSITORY",
		"OMNIGREX_LIVE_GITHUB_PULL_REQUEST",
		"OMNIGREX_LIVE_GITHUB_PULL_REQUEST_HEAD_SHA",
		"OMNIGREX_LIVE_GITHUB_DEVELOPER_APP_ID",
		"OMNIGREX_LIVE_GITHUB_DEVELOPER_PRIVATE_KEY_FILE",
		"OMNIGREX_LIVE_GITHUB_REVIEWER_APP_ID",
		"OMNIGREX_LIVE_GITHUB_REVIEWER_PRIVATE_KEY_FILE",
	}
	values := make(map[string]string, len(required))
	missing := make([]string, 0)
	for _, name := range required {
		values[name] = os.Getenv(name)
		if values[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("live GitHub test is not configured; missing %s", strings.Join(missing, ", "))
	}

	developer := liveAppSigner(t, values["OMNIGREX_LIVE_GITHUB_DEVELOPER_APP_ID"], values["OMNIGREX_LIVE_GITHUB_DEVELOPER_PRIVATE_KEY_FILE"])
	reviewer := liveAppSigner(t, values["OMNIGREX_LIVE_GITHUB_REVIEWER_APP_ID"], values["OMNIGREX_LIVE_GITHUB_REVIEWER_PRIVATE_KEY_FILE"])
	pullRequestNumber, err := strconv.Atoi(values["OMNIGREX_LIVE_GITHUB_PULL_REQUEST"])
	if err != nil || pullRequestNumber <= 0 {
		t.Fatalf("OMNIGREX_LIVE_GITHUB_PULL_REQUEST must be a positive integer")
	}
	client, err := githubapi.NewAPIClient(nil, os.Getenv("OMNIGREX_LIVE_GITHUB_API_URL"))
	if err != nil {
		t.Fatalf("NewAPIClient() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner := values["OMNIGREX_LIVE_GITHUB_OWNER"]
	repository := values["OMNIGREX_LIVE_GITHUB_REPOSITORY"]
	developerToken := liveInstallationToken(t, ctx, client, developer, owner, repository)
	if developerToken == "" {
		t.Fatal("Developer App returned an empty installation token")
	}
	reviewerToken := liveInstallationToken(t, ctx, client, reviewer, owner, repository)

	for _, event := range []githubapi.ReviewEvent{githubapi.ReviewApprove, githubapi.ReviewRequestChanges} {
		t.Run(string(event), func(t *testing.T) {
			review, err := client.SubmitReview(ctx, reviewerToken, owner, repository, pullRequestNumber, githubapi.ReviewRequest{
				Body:     fmt.Sprintf("Omnigrex live GitHub App API test: %s.", event),
				CommitID: values["OMNIGREX_LIVE_GITHUB_PULL_REQUEST_HEAD_SHA"],
				Event:    event,
			})
			if err != nil {
				t.Fatalf("SubmitReview(%s) error = %v", event, err)
			}
			if review.ID <= 0 {
				t.Errorf("SubmitReview(%s) returned invalid review ID %d", event, review.ID)
			}
		})
	}
}

func liveAppSigner(t *testing.T, appIDValue, privateKeyFile string) *githubapi.AppJWTSigner {
	t.Helper()
	appID, err := strconv.ParseInt(appIDValue, 10, 64)
	if err != nil || appID <= 0 {
		t.Fatal("live GitHub App ID must be a positive integer")
	}
	privateKey, err := os.ReadFile(privateKeyFile)
	if err != nil {
		t.Fatalf("read live GitHub App private key file: %v", err)
	}
	signer, err := githubapi.NewAppJWTSigner(appID, privateKey, nil)
	if err != nil {
		t.Fatalf("NewAppJWTSigner() error = %v", err)
	}
	return signer
}

func liveInstallationToken(t *testing.T, ctx context.Context, client *githubapi.APIClient, signer *githubapi.AppJWTSigner, owner, repository string) string {
	t.Helper()
	appJWT, err := signer.AppJWT(ctx)
	if err != nil {
		t.Fatalf("AppJWT() error = %v", err)
	}
	installationID, err := client.ResolveRepositoryInstallation(ctx, appJWT, owner, repository)
	if err != nil {
		t.Fatalf("ResolveRepositoryInstallation() error = %v", err)
	}
	cache := githubapi.NewInstallationTokenCache(signer, client, nil)
	token, err := cache.Token(ctx, installationID)
	if err != nil {
		t.Fatalf("installation Token() error = %v", err)
	}
	return token
}
