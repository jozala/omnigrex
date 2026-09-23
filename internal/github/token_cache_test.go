package github_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

type appJWTProviderFunc func(context.Context) (string, error)

func (provider appJWTProviderFunc) AppJWT(ctx context.Context) (string, error) {
	return provider(ctx)
}

type installationTokenRequesterFunc func(context.Context, string, int64) (githubapi.InstallationToken, error)

func (requester installationTokenRequesterFunc) CreateInstallationToken(ctx context.Context, jwt string, installationID int64) (githubapi.InstallationToken, error) {
	return requester(ctx, jwt, installationID)
}

type mutableClock struct {
	mutex sync.Mutex
	now   time.Time
}

type observedContext struct {
	context.Context
	once    sync.Once
	checked chan struct{}
}

func (ctx *observedContext) Err() error {
	ctx.once.Do(func() { close(ctx.checked) })
	return ctx.Context.Err()
}

func (clock *mutableClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *mutableClock) Set(now time.Time) {
	clock.mutex.Lock()
	clock.now = now
	clock.mutex.Unlock()
}

func TestInstallationTokenCacheCachesByInstallationAndRefreshesBeforeExpiry(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{now: now}
	jwtCalls := 0
	requestCalls := 0
	cache := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) {
			jwtCalls++
			return "app-jwt", nil
		}),
		installationTokenRequesterFunc(func(_ context.Context, jwt string, installationID int64) (githubapi.InstallationToken, error) {
			requestCalls++
			if jwt != "app-jwt" {
				t.Errorf("app JWT = %q, want app-jwt", jwt)
			}
			return githubapi.InstallationToken{
				Token:     fmt.Sprintf("installation-%d-token-%d", installationID, requestCalls),
				ExpiresAt: clock.Now().Add(10 * time.Minute),
			}, nil
		}),
		clock,
	)

	first, err := cache.Token(context.Background(), 41)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	again, err := cache.Token(context.Background(), 41)
	if err != nil {
		t.Fatalf("cached Token() error = %v", err)
	}
	other, err := cache.Token(context.Background(), 42)
	if err != nil {
		t.Fatalf("other installation Token() error = %v", err)
	}
	if first != "installation-41-token-1" || again != first || other != "installation-42-token-2" {
		t.Errorf("tokens = %q, %q, %q", first, again, other)
	}
	if requestCalls != 2 || jwtCalls != 2 {
		t.Fatalf("calls before refresh = requester %d, JWT %d; want 2 each", requestCalls, jwtCalls)
	}

	clock.Set(now.Add(9*time.Minute + time.Second))
	refreshed, err := cache.Token(context.Background(), 41)
	if err != nil {
		t.Fatalf("refresh Token() error = %v", err)
	}
	if refreshed != "installation-41-token-3" {
		t.Errorf("refreshed token = %q, want installation-41-token-3", refreshed)
	}
	if requestCalls != 3 || jwtCalls != 3 {
		t.Errorf("calls after refresh = requester %d, JWT %d; want 3 each", requestCalls, jwtCalls)
	}
}

func TestInstallationTokenCacheCollapsesConcurrentRequests(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int64
	cache := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) { return "app-jwt", nil }),
		installationTokenRequesterFunc(func(context.Context, string, int64) (githubapi.InstallationToken, error) {
			if requests.Add(1) == 1 {
				close(started)
			}
			<-release
			return githubapi.InstallationToken{Token: "shared-token", ExpiresAt: now.Add(time.Hour)}, nil
		}),
		fixedClock{now: now},
	)

	const callers = 24
	results := make(chan string, callers)
	errorsFound := make(chan error, callers)
	for range callers {
		go func() {
			token, err := cache.Token(context.Background(), 91)
			results <- token
			errorsFound <- err
		}()
	}
	<-started
	close(release)
	for range callers {
		if err := <-errorsFound; err != nil {
			t.Errorf("Token() error = %v", err)
		}
		if token := <-results; token != "shared-token" {
			t.Errorf("Token() = %q, want shared-token", token)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("installation token requests = %d, want 1", got)
	}
}

func TestInstallationTokenCacheWaitCanBeCanceled(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	cache := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) { return "app-jwt", nil }),
		installationTokenRequesterFunc(func(context.Context, string, int64) (githubapi.InstallationToken, error) {
			close(started)
			<-release
			return githubapi.InstallationToken{Token: "shared-token", ExpiresAt: now.Add(time.Hour)}, nil
		}),
		fixedClock{now: now},
	)
	first := make(chan error, 1)
	go func() {
		_, err := cache.Token(context.Background(), 91)
		first <- err
	}()
	<-started

	baseContext, cancel := context.WithCancel(context.Background())
	waitingContext := &observedContext{Context: baseContext, checked: make(chan struct{})}
	waiting := make(chan error, 1)
	go func() {
		_, err := cache.Token(waitingContext, 91)
		waiting <- err
	}()
	<-waitingContext.checked
	cancel()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiting Token() error = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(release)
		<-first
		t.Fatal("waiting Token() did not return when its context was canceled")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
}

func TestInstallationTokenCacheDoesNotCacheErrors(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	requests := 0
	cache := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) { return "app-jwt", nil }),
		installationTokenRequesterFunc(func(context.Context, string, int64) (githubapi.InstallationToken, error) {
			requests++
			if requests == 1 {
				return githubapi.InstallationToken{}, errors.New("temporary failure")
			}
			return githubapi.InstallationToken{Token: "recovered-token", ExpiresAt: now.Add(time.Hour)}, nil
		}),
		fixedClock{now: now},
	)

	if _, err := cache.Token(context.Background(), 91); err == nil {
		t.Fatal("first Token() error = nil, want temporary failure")
	}
	token, err := cache.Token(context.Background(), 91)
	if err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if token != "recovered-token" || requests != 2 {
		t.Errorf("second Token() = %q after %d requests, want recovered-token after 2", token, requests)
	}
}

func TestInstallationTokenCacheRejectsAndDoesNotCacheInvalidTokens(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	requests := 0
	cache := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) { return "app-jwt", nil }),
		installationTokenRequesterFunc(func(context.Context, string, int64) (githubapi.InstallationToken, error) {
			requests++
			if requests == 1 {
				return githubapi.InstallationToken{ExpiresAt: now.Add(time.Hour)}, nil
			}
			return githubapi.InstallationToken{Token: "valid-token", ExpiresAt: now.Add(time.Hour)}, nil
		}),
		fixedClock{now: now},
	)

	if _, err := cache.Token(context.Background(), 91); !errors.Is(err, githubapi.ErrInvalidInstallationToken) {
		t.Fatalf("first Token() error = %v, want ErrInvalidInstallationToken", err)
	}
	token, err := cache.Token(context.Background(), 91)
	if err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if token != "valid-token" || requests != 2 {
		t.Errorf("second Token() = %q after %d requests, want valid-token after 2", token, requests)
	}
}

func TestInstallationTokenCachesKeepDeveloperAndReviewerCredentialsSeparate(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	requester := installationTokenRequesterFunc(func(_ context.Context, jwt string, _ int64) (githubapi.InstallationToken, error) {
		return githubapi.InstallationToken{Token: jwt + "-installation-token", ExpiresAt: now.Add(time.Hour)}, nil
	})
	developer := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) { return "developer", nil }),
		requester,
		fixedClock{now: now},
	)
	reviewer := githubapi.NewInstallationTokenCache(
		appJWTProviderFunc(func(context.Context) (string, error) { return "reviewer", nil }),
		requester,
		fixedClock{now: now},
	)

	developerToken, err := developer.Token(context.Background(), 91)
	if err != nil {
		t.Fatalf("developer Token() error = %v", err)
	}
	reviewerToken, err := reviewer.Token(context.Background(), 91)
	if err != nil {
		t.Fatalf("reviewer Token() error = %v", err)
	}
	if developerToken != "developer-installation-token" || reviewerToken != "reviewer-installation-token" {
		t.Errorf("tokens = %q and %q, want separate Developer and Reviewer credentials", developerToken, reviewerToken)
	}
}
