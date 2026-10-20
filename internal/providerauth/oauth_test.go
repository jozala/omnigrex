package providerauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOAuthValidatesCallbackIdentityAndPermissions(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "nonce", "issuer", "audience", "expiry", "signature", "scope", "state", "callback", "duplicate-state", "oauth-error"} {
		t.Run(scenario, func(t *testing.T) {
			var p pendingLogin
			var issuer string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/keys" {
					_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
					return
				}
				calls++
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "oaiapp_fixture" || r.Form.Get("resource") != resource || r.Form.Get("redirect_uri") != p.RedirectURI || r.Form.Get("code_verifier") != p.Verifier {
					t.Error("incorrect code exchange")
				}
				claims := map[string]any{"iss": issuer, "sub": "verified-subject", "aud": "oaiapp_fixture", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.Nonce, "email": "operator@example.test"}
				switch scenario {
				case "nonce":
					claims["nonce"] = "wrong"
				case "issuer":
					claims["iss"] = "https://wrong.example"
				case "audience":
					claims["aud"] = "oaiapp_other"
				case "expiry":
					claims["exp"] = time.Now().Add(-time.Hour).Unix()
				}
				id := signIdentity(t, key, claims)
				if scenario == "signature" {
					id += "broken"
				}
				scope := "openid offline_access resource.invoke " + directScope
				if scenario == "scope" {
					scope = "openid"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-fixture", "refresh_token": "refresh-fixture", "token_type": "Bearer", "expires_in": 3600, "scope": scope, "id_token": id})
			}))
			defer server.Close()
			issuer = server.URL
			oauth := &openAI{client: server.Client(), issuer: issuer, authorizeURL: issuer + "/authorize", tokenURL: issuer + "/token", jwksURL: issuer + "/keys"}
			m, err := newManager(t.TempDir(), oauth)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			start, err := m.BeginLogin("http://127.0.0.1:1455/auth/callback", true)
			if err != nil {
				t.Fatal(err)
			}
			p = *m.pending
			authURL, _ := url.Parse(start.URL)
			challenge := sha256.Sum256([]byte(p.Verifier))
			if authURL.Query().Get("agent_name_hint") != "Omnigrex" || authURL.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || authURL.Query().Get("client_id") != dynamicClient {
				t.Fatal("incorrect authorization request")
			}
			callback := p.RedirectURI + "?code=one-time&state=" + start.ID + "&client_id=oaiapp_fixture"
			switch scenario {
			case "state":
				callback = strings.ReplaceAll(callback, start.ID, "wrong-state")
			case "callback":
				callback = strings.ReplaceAll(callback, "127.0.0.1", "localhost")
			case "duplicate-state":
				callback += "&state=" + start.ID
			case "oauth-error":
				callback += "&error=access_denied"
			}
			err = m.CompleteLogin(context.Background(), start.ID, callback)
			if scenario == "valid" {
				if err != nil || !m.Status().SignedIn || m.Status().Subject != "verified-subject" {
					t.Fatalf("valid login failed: %v", err)
				}
				if err := m.CompleteLogin(context.Background(), start.ID, callback); err == nil {
					t.Fatal("reused login attempt")
				}
				return
			}
			if err == nil || m.Status().SignedIn {
				t.Fatal("unverified login accepted")
			}
			if (scenario == "state" || scenario == "callback" || scenario == "duplicate-state" || scenario == "oauth-error") && calls != 0 {
				t.Fatal("exchanged code from invalid callback")
			}
		})
	}
}

func signIdentity(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "fixture", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestFailedReauthorizationPreservesWorkingLogin(t *testing.T) {
	m := signedIn(t, t.TempDir(), &fixtureOAuth{})
	m.state.Credential.ExpiresAt = time.Now().Add(time.Hour)
	start, err := m.BeginLogin("http://127.0.0.1:1455/auth/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.pending.ClientID != "oaiapp_fixture" {
		t.Fatal("returning login did not reuse registration")
	}
	if err := m.CompleteLogin(context.Background(), start.ID, "invalid"); err == nil {
		t.Fatal("invalid login accepted")
	}
	token, err := m.Access(context.Background())
	if err != nil || token.Access != "old-access" {
		t.Fatal("failed login replaced active credentials")
	}
}

func TestRefreshUsesIssuedClientAndRecognizesTerminalErrors(t *testing.T) {
	for _, code := range []string{"invalid_grant", "invalid_client", "invalid_refresh_token", "refresh_token_reused", "temporary"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if r.Form.Get("client_id") != "oaiapp_existing" || r.Form.Get("refresh_token") != "existing-refresh" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Has("scope") {
					t.Error("incorrect refresh grant")
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "secret-provider-diagnostic"})
			}))
			defer server.Close()
			o := &openAI{client: server.Client(), tokenURL: server.URL}
			_, err := o.renew(context.Background(), credential{ClientID: "oaiapp_existing", Refresh: "existing-refresh"})
			if code == "temporary" && err != ErrTemporary || code != "temporary" && err != ErrAuthentication {
				t.Fatalf("classification = %v", err)
			}
			if strings.Contains(err.Error(), "secret-provider-diagnostic") {
				t.Fatal("retained provider diagnostics")
			}
		})
	}
}
