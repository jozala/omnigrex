package providerauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

const (
	openAIIssuer  = "https://auth.openai.com"
	resource      = "https://api.openai.com/v1"
	directScope   = "chatgpt.tokens.use.direct"
	dynamicClient = "dynamic_agent_client"
)

type pendingLogin struct {
	State, Nonce, Verifier, RedirectURI, ClientID, Subject string
	Generation                                             string
	Expires                                                time.Time
}

// LoginStart contains a one-time operator-visible authorization URL, not tokens.
type LoginStart struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func randomValue() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// BeginLogin can use the current registration or register a different account.
// Both choices are ordinary login; neither waits for active Agent Turns.
func (m *Manager) BeginLogin(redirect string, newAccount bool) (LoginStart, error) {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/auth/callback" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return LoginStart{}, ErrLogin
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.broken {
		return LoginStart{}, ErrStorage
	}
	p := pendingLogin{State: randomValue(), Nonce: randomValue(), Verifier: randomValue(), RedirectURI: redirect, ClientID: dynamicClient, Generation: m.state.Generation, Expires: time.Now().Add(10 * time.Minute)}
	if !newAccount {
		if r := m.state.Registration; r != nil {
			p.ClientID, p.Subject = r.ClientID, r.Subject
		}
		if c := m.state.Credential; c != nil {
			p.ClientID, p.Subject = c.ClientID, c.Subject
		}
	}
	m.pending = &p
	return LoginStart{ID: p.State, URL: m.oauth.authorization(p, m.state.HostID)}, nil
}

func (m *Manager) CompleteLogin(ctx context.Context, id, callback string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pending
	if p == nil || subtle.ConstantTimeCompare([]byte(p.State), []byte(id)) != 1 {
		return ErrLogin
	}
	m.pending = nil // One-time consumption includes failed callbacks and exchange.
	if m.closed || m.broken {
		return ErrStorage
	}
	if time.Now().After(p.Expires) || p.Generation != m.state.Generation || ctx.Err() != nil {
		return ErrLogin
	}
	loginCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	c, err := m.oauth.exchange(loginCtx, *p, callback)
	if err != nil {
		return ErrLogin
	}
	next := m.state
	next.Credential, next.AuthBlocked, next.RefreshPending, next.Generation = &c, false, false, uuid.NewString()
	next.Registration = &registration{ClientID: c.ClientID, Subject: c.Subject}
	if err := m.save(next); err != nil {
		return err
	}
	m.nextRefresh, m.backoff = time.Time{}, 0
	return nil
}

type openAI struct {
	client                                  *http.Client
	issuer, authorizeURL, tokenURL, jwksURL string
}

func newOpenAI() *openAI {
	return &openAI{
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		issuer: openAIIssuer, authorizeURL: openAIIssuer + "/api/accounts/authorize", tokenURL: openAIIssuer + "/api/accounts/oauth/token", jwksURL: openAIIssuer + "/.well-known/jwks.json",
	}
}

func (o *openAI) authorization(p pendingLogin, host string) string {
	challenge := sha256.Sum256([]byte(p.Verifier))
	q := url.Values{
		"client_id": {p.ClientID}, "ext_agent_host_id": {host}, "response_type": {"code"},
		"redirect_uri": {p.RedirectURI}, "resource": {resource},
		"scope": {"openid profile email offline_access resource.invoke " + directScope},
		"state": {p.State}, "nonce": {p.Nonce}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}
	if p.ClientID == dynamicClient {
		q.Set("agent_name_hint", "Omnigrex")
	}
	// ID-token hints are optional. Omitting them keeps tokens out of CLI URLs
	// and allows the browser account selector to remain visible.
	return o.authorizeURL + "?" + q.Encode()
}

type tokenResponse struct {
	Access   string          `json:"access_token"`
	Refresh  string          `json:"refresh_token"`
	IDToken  string          `json:"id_token"`
	Type     string          `json:"token_type"`
	Expires  int64           `json:"expires_in"`
	Scope    string          `json:"scope"`
	Earliest json.RawMessage `json:"earliest_refresh_at"`
	Error    string          `json:"error"`
}

func tokenString(s string) bool {
	return s != "" && len(s) <= 64<<10 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

func (o *openAI) request(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, ErrTemporary
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := o.client.Do(req)
	if err != nil {
		return tokenResponse{}, ErrTemporary
	}
	defer response.Body.Close()
	var token tokenResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 256<<10))
	if decoder.Decode(&token) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return tokenResponse{}, ErrTemporary
	}
	if response.StatusCode != http.StatusOK {
		switch token.Error {
		case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused", "invalid_client":
			return tokenResponse{}, ErrAuthentication
		default:
			return tokenResponse{}, ErrTemporary
		}
	}
	return token, nil
}

func fromToken(t tokenResponse, c credential) (credential, error) {
	if !tokenString(t.Access) || strings.HasPrefix(t.Access, "sk-") || !tokenString(t.Refresh) || !strings.EqualFold(t.Type, "Bearer") || t.Expires <= 0 || t.Expires > 7*24*3600 {
		return credential{}, ErrTemporary
	}
	scopes := strings.Fields(t.Scope)
	if !slices.Contains(scopes, directScope) {
		return credential{}, ErrAuthentication
	}
	c.Access, c.Refresh, c.Scopes, c.ExpiresAt = t.Access, t.Refresh, scopes, time.Now().Add(time.Duration(t.Expires)*time.Second)
	c.EarliestRefresh = time.Time{}
	if len(t.Earliest) != 0 && string(t.Earliest) != "null" {
		var seconds int64
		var date string
		if json.Unmarshal(t.Earliest, &seconds) == nil {
			c.EarliestRefresh = time.Unix(seconds, 0)
		} else if json.Unmarshal(t.Earliest, &date) == nil {
			var err error
			c.EarliestRefresh, err = time.Parse(time.RFC3339, date)
			if err != nil {
				return credential{}, ErrTemporary
			}
		} else {
			return credential{}, ErrTemporary
		}
	}
	return c, nil
}

func (o *openAI) exchange(ctx context.Context, p pendingLogin, callback string) (credential, error) {
	u, err := url.Parse(callback)
	expected, _ := url.Parse(p.RedirectURI)
	if err != nil || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || u.User != nil || u.Fragment != "" {
		return credential{}, ErrLogin
	}
	q := u.Query()
	if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(p.State)) != 1 || q.Has("error") || len(q["code"]) != 1 || !tokenString(q.Get("code")) || len(q["client_id"]) > 1 {
		return credential{}, ErrLogin
	}
	clientID := p.ClientID
	if clientID == dynamicClient {
		clientID = q.Get("client_id")
		if clientID == dynamicClient || len(clientID) > 256 || !tokenString(clientID) {
			return credential{}, ErrLogin
		}
	} else if q.Has("client_id") && q.Get("client_id") != clientID {
		return credential{}, ErrLogin
	}
	t, err := o.request(ctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {q.Get("code")}, "code_verifier": {p.Verifier}, "redirect_uri": {p.RedirectURI}, "resource": {resource}})
	if err != nil {
		return credential{}, ErrLogin
	}
	verifyCtx := oidc.ClientContext(ctx, o.client)
	verifier := oidc.NewVerifier(o.issuer, oidc.NewRemoteKeySet(verifyCtx, o.jwksURL), &oidc.Config{ClientID: clientID})
	identity, err := verifier.Verify(verifyCtx, t.IDToken)
	if err != nil || identity.Subject == "" || identity.Nonce != p.Nonce || p.Subject != "" && identity.Subject != p.Subject {
		return credential{}, ErrLogin
	}
	var claims struct {
		Email string `json:"email"`
	}
	if identity.Claims(&claims) != nil || strings.ContainsAny(claims.Email, "\x00\r\n") {
		return credential{}, ErrLogin
	}
	c, err := fromToken(t, credential{ClientID: clientID, Subject: identity.Subject, Email: claims.Email, IDToken: t.IDToken})
	if err != nil {
		return credential{}, ErrLogin
	}
	return c, nil
}

func (o *openAI) renew(ctx context.Context, c credential) (credential, error) {
	t, err := o.request(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.Refresh}, "resource": {resource}})
	if err != nil {
		return credential{}, err
	}
	// Preserve the previously verified identity and its ID token. Refresh need
	// not return a new ID token and never changes account identity.
	return fromToken(t, c)
}

func (o *openAI) revoke(ctx context.Context, c credential) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return false
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return false
	}
	var configuration struct {
		Issuer     string `json:"issuer"`
		Revocation string `json:"revocation_endpoint"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&configuration)
	resp.Body.Close()
	u, parseErr := url.Parse(configuration.Revocation)
	issuer, _ := url.Parse(o.issuer)
	if decodeErr != nil || resp.StatusCode != 200 || configuration.Issuer != o.issuer || parseErr != nil || u.Scheme != issuer.Scheme || u.Host != issuer.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	form := url.Values{"token": {c.Refresh}, "token_type_hint": {"refresh_token"}, "client_id": {c.ClientID}}
	for attempt := 0; attempt < 3; attempt++ {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err = o.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
			if resp.StatusCode < 500 {
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return false
}
