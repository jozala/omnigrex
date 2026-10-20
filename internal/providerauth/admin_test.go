package providerauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperatorCommandsUseRunningOwnerAndOfflineCommandsAcquireExclusiveOwnership(t *testing.T) {
	dir, err := os.MkdirTemp("", "auth-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := signedIn(t, dir, &fixtureOAuth{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.ServeAdmin(ctx, dir) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := NewAdminClient(dir)
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := client.Status(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admin socket did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	operator, closeOperator, err := OpenOperator(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOperator()
	status, err := operator.Status(ctx)
	if err != nil || !status.SignedIn || status.Subject != "subject" {
		t.Fatal("operator did not reach running owner")
	}
	info, err := os.Stat(filepath.Join(dir, "admin.sock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("admin socket is not owner-only")
	}
	if _, err := operator.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = operator.Status(ctx)
	if err != nil || status.SignedIn {
		t.Fatal("logout did not remove login")
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "old-access") || strings.Contains(string(encoded), "old-refresh") {
		t.Fatal("status exposed credentials")
	}
}

func TestOfflineAuthenticationDoesNotNeedConfiguredGitHubOrPi(t *testing.T) {
	operator, closeOperator, err := OpenOperator(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeOperator()
	start, err := operator.BeginLogin(context.Background(), LoginRequest{RedirectURI: "http://127.0.0.1:1455/auth/callback"})
	if err != nil || !strings.Contains(start.URL, "agent_name_hint=Omnigrex") {
		t.Fatal("standalone login is unavailable")
	}
}

func TestAdminRejectsBrowserOriginsAndMalformedInputsWithoutEcho(t *testing.T) {
	m := signedIn(t, t.TempDir(), &fixtureOAuth{})
	request := httptest.NewRequest(http.MethodPost, "/login/complete", strings.NewReader(`{"id":"provider-secret","callback":"provider-secret","unexpected":true}`))
	response := httptest.NewRecorder()
	m.serveAdmin(response, request)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "provider-secret") {
		t.Fatal("malformed admin input was accepted or echoed")
	}
	request = httptest.NewRequest(http.MethodPost, "/logout", nil)
	request.Header.Set("Origin", "https://unrelated.example")
	response = httptest.NewRecorder()
	m.serveAdmin(response, request)
	if response.Code != http.StatusForbidden || !m.Status().SignedIn {
		t.Fatal("browser origin changed login")
	}
}
