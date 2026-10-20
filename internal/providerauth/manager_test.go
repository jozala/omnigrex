package providerauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureOAuth struct {
	refreshes  atomic.Int32
	refresh    func(credential) (credential, error)
	exchangeFn func(pendingLogin, string) (credential, error)
}

func (f *fixtureOAuth) authorization(p pendingLogin, host string) string {
	return "https://example.test/authorize?state=" + p.State
}
func (f *fixtureOAuth) exchange(_ context.Context, p pendingLogin, callback string) (credential, error) {
	if f.exchangeFn != nil {
		return f.exchangeFn(p, callback)
	}
	return credential{}, ErrLogin
}
func (f *fixtureOAuth) renew(_ context.Context, c credential) (credential, error) {
	f.refreshes.Add(1)
	if f.refresh != nil {
		return f.refresh(c)
	}
	c.Access, c.Refresh = "renewed-access", "rotated-refresh"
	c.ExpiresAt = time.Now().Add(time.Hour)
	return c, nil
}
func (f *fixtureOAuth) revoke(context.Context, credential) bool { return true }

func signedIn(t *testing.T, dir string, oauth oauthProvider) *Manager {
	t.Helper()
	m, err := newManager(dir, oauth)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := m.state
	s.Credential = &credential{ClientID: "oaiapp_fixture", Subject: "subject", Email: "operator@example.test", Access: "old-access", Refresh: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConcurrentRefreshPersistsLatestCredentials(t *testing.T) {
	dir := t.TempDir()
	oauth := &fixtureOAuth{}
	m := signedIn(t, dir, oauth)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			token, err := m.Access(context.Background())
			if err != nil || token.Access != "renewed-access" {
				t.Errorf("Access: %v, %v", token.Access != "renewed-access", err)
			}
		})
	}
	wg.Wait()
	if oauth.refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d", oauth.refreshes.Load())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newManager(dir, oauth)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.state.Credential.Refresh != "rotated-refresh" {
		t.Fatal("rotated refresh was not durable")
	}
	info, err := os.Stat(filepath.Join(dir, "chatgpt.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file is not private")
	}
}

func TestIndependentBlocksSurviveRestartAndRequireExplicitRecovery(t *testing.T) {
	dir := t.TempDir()
	m := signedIn(t, dir, &fixtureOAuth{})
	generation := m.state.Generation
	if err := m.Report(generation, QuotaFailure); err != nil {
		t.Fatal(err)
	}
	if err := m.Report(generation, AuthFailure); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := newManager(dir, &fixtureOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Access(context.Background()); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("Access = %v", err)
	}
	// Successful login clears authentication, not a separately reported quota block.
	m.mu.Lock()
	next := m.state
	next.AuthBlocked = false
	err = m.save(next)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Access(context.Background()); !errors.Is(err, ErrQuota) {
		t.Fatalf("Access = %v", err)
	}
	if err := m.Unblock(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Access(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStaleFailureCannotBlockReplacementAccount(t *testing.T) {
	m := signedIn(t, t.TempDir(), &fixtureOAuth{})
	if err := m.Report("old-generation", QuotaFailure); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
	if err := m.Report(m.state.Generation, "arbitrary-provider-text"); err == nil {
		t.Fatal("accepted unclassified provider text")
	}
}

func TestInterruptedRefreshRequiresReauthorization(t *testing.T) {
	dir := t.TempDir()
	m := signedIn(t, dir, &fixtureOAuth{})
	s := m.state
	s.RefreshPending = true
	if err := m.save(s); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	m, err := newManager(dir, &fixtureOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Check(); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("Check = %v", err)
	}
}

func TestTemporaryRefreshPreservesLoginAndBacksOff(t *testing.T) {
	oauth := &fixtureOAuth{refresh: func(credential) (credential, error) { return credential{}, ErrTemporary }}
	m := signedIn(t, t.TempDir(), oauth)
	for range 3 {
		if _, err := m.Access(context.Background()); !errors.Is(err, ErrTemporary) {
			t.Fatalf("Access = %v", err)
		}
	}
	if oauth.refreshes.Load() != 1 || m.state.Credential.Refresh != "old-refresh" || m.state.AuthBlocked {
		t.Fatal("temporary error lost login or bypassed backoff")
	}
}

func TestLogoutSerializesAgainstRefreshAndRemovesTokens(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	oauth := &fixtureOAuth{refresh: func(c credential) (credential, error) {
		close(started)
		<-release
		c.Access, c.Refresh, c.ExpiresAt = "new-access", "new-refresh", time.Now().Add(time.Hour)
		return c, nil
	}}
	m := signedIn(t, t.TempDir(), oauth)
	done := make(chan struct{})
	go func() { _, _ = m.Access(context.Background()); close(done) }()
	<-started
	loggedOut := make(chan struct{})
	go func() { _, _ = m.Logout(context.Background()); close(loggedOut) }()
	close(release)
	<-done
	<-loggedOut
	if m.state.Credential != nil || !errors.Is(m.Check(), ErrAuthentication) {
		t.Fatal("refresh resurrected logged-out credentials")
	}
}

func TestOnlyOneCredentialWriter(t *testing.T) {
	dir := t.TempDir()
	m, err := newManager(dir, &fixtureOAuth{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if other, err := newManager(dir, &fixtureOAuth{}); err == nil {
		other.Close()
		t.Fatal("two credential owners admitted")
	}
}

func TestIdleMaintenanceRenewsWithoutWorkItems(t *testing.T) {
	oauth := &fixtureOAuth{}
	m := signedIn(t, t.TempDir(), oauth)
	m.maintain(context.Background())
	if oauth.refreshes.Load() != 1 || m.state.Credential.Access != "renewed-access" {
		t.Fatal("idle maintenance did not renew")
	}
}

func TestAccountReplacementWaitsForRefreshButNotActiveAgentTurns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	oauth := &fixtureOAuth{refresh: func(c credential) (credential, error) {
		close(started)
		<-release
		c.Refresh, c.ExpiresAt = "old-account-rotated", time.Now().Add(time.Hour)
		return c, nil
	}, exchangeFn: func(p pendingLogin, _ string) (credential, error) {
		if p.ClientID != dynamicClient {
			t.Error("different account must use its own registration")
		}
		return credential{ClientID: "oaiapp_new", Subject: "new-subject", Access: "new-account-access", Refresh: "new-account-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	m := signedIn(t, t.TempDir(), oauth)
	oldGeneration := m.state.Generation
	start, err := m.BeginLogin("http://127.0.0.1:1455/auth/callback", true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = m.Access(context.Background()); close(done) }()
	<-started
	login := make(chan error, 1)
	go func() { login <- m.CompleteLogin(context.Background(), start.ID, "callback") }()
	close(release)
	<-done
	if err := <-login; err != nil {
		t.Fatal(err)
	}
	token, err := m.Access(context.Background())
	if err != nil || token.Access != "new-account-access" || m.Status().Subject != "new-subject" {
		t.Fatal("refresh overwrote replacement account")
	}
	if err := m.Report(oldGeneration, AuthFailure); err != nil || m.Check() != nil {
		t.Fatal("old account failure blocked replacement")
	}
}

func TestFailedPersistenceNeverReleasesRenewedToken(t *testing.T) {
	m := signedIn(t, t.TempDir(), &fixtureOAuth{})
	_ = m.store.root.Close()
	token, err := m.Access(context.Background())
	if !errors.Is(err, ErrStorage) || token.Access != "" || !errors.Is(m.Check(), ErrStorage) {
		t.Fatal("released a token without durable credential ownership")
	}
}

func TestFreshAccessRemainsUsableDuringBoundedRefreshBackoff(t *testing.T) {
	oauth := &fixtureOAuth{refresh: func(credential) (credential, error) { return credential{}, ErrTemporary }}
	m := signedIn(t, t.TempDir(), oauth)
	m.state.Credential.ExpiresAt = time.Now().Add(4 * time.Minute)
	for range 2 {
		token, err := m.Access(context.Background())
		if err != nil || token.Access != "old-access" {
			t.Fatalf("unexpired access unavailable: %v", err)
		}
	}
	if oauth.refreshes.Load() != 1 {
		t.Fatal("backoff was bypassed")
	}
}
