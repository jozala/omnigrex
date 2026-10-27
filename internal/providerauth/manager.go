// Package providerauth owns the installation's mutable ChatGPT login.
// Only access tokens cross the controller/runtime boundary.
package providerauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	QuotaFailure         = "subscription_quota"
	AuthFailure          = "subscription_auth"
	TemporaryFailure     = "subscription_temporary"
	ConfigurationFailure = "subscription_configuration"
	refreshMargin        = 5 * time.Minute
)

var (
	ErrAuthentication   = errors.New("subscription_auth: run omnigrex auth login")
	ErrQuota            = errors.New("subscription_quota: check ChatGPT Settings > Usage, then run omnigrex auth unblock and explicitly resume affected Work Items")
	ErrRequestTemporary = errors.New("subscription_temporary: subscription request failed; source not established by classification")
	ErrTemporary        = errors.New("subscription_temporary: ChatGPT authentication is temporarily unavailable")
	ErrStorage          = errors.New("ChatGPT credential storage is unavailable")
	ErrLogin            = errors.New("ChatGPT sign-in could not be verified; start a fresh login")
	ErrConfiguration    = errors.New("subscription_configuration: the selected model or request is not supported by this ChatGPT account; check the Agent Profile and provider configuration")
)

// Failure returns only closed-vocabulary, credential-free failure codes.
func Failure(err error) string {
	switch {
	case errors.Is(err, ErrAuthentication):
		return AuthFailure
	case errors.Is(err, ErrQuota):
		return QuotaFailure
	default:
		return TemporaryFailure
	}
}

type credential struct {
	ClientID        string    `json:"client_id"`
	Subject         string    `json:"subject"`
	Email           string    `json:"email,omitempty"`
	IDToken         string    `json:"id_token"`
	Access          string    `json:"access_token"`
	Refresh         string    `json:"refresh_token"`
	Scopes          []string  `json:"scopes"`
	ExpiresAt       time.Time `json:"expires_at"`
	EarliestRefresh time.Time `json:"earliest_refresh_at,omitempty"`
}

type savedState struct {
	Version        int           `json:"version"`
	HostID         string        `json:"host_id"`
	Generation     string        `json:"generation"`
	Credential     *credential   `json:"credential,omitempty"`
	Registration   *registration `json:"registration,omitempty"`
	AuthBlocked    bool          `json:"auth_blocked"`
	QuotaBlocked   bool          `json:"quota_blocked"`
	RefreshPending bool          `json:"refresh_pending"`
}

type registration struct {
	ClientID string `json:"client_id"`
	Subject  string `json:"subject"`
}

// AccessToken is returned only on the authenticated private runtime endpoint.
type AccessToken struct {
	Access     string    `json:"access_token"`
	ExpiresAt  time.Time `json:"expires_at"`
	Generation string    `json:"generation"`
}

// Status deliberately contains no access, refresh, ID token, or pending login.
type Status struct {
	Mode             string `json:"mode"`
	SignedIn         bool   `json:"signed_in"`
	Subject          string `json:"subject,omitempty"`
	Email            string `json:"email,omitempty"`
	AuthBlocked      bool   `json:"reauthorization_required"`
	QuotaBlocked     bool   `json:"quota_blocked"`
	StorageAvailable bool   `json:"storage_available"`
}

type oauthProvider interface {
	authorization(pendingLogin, string) string
	exchange(context.Context, pendingLogin, string) (credential, error)
	renew(context.Context, credential) (credential, error)
	revoke(context.Context, credential) bool
}

// Manager is the only writer for an installation credential directory.
// The mutex covers remote refresh and durable replacement, including login and
// logout. A caller cancellation cannot abandon an already-started rotation.
type Manager struct {
	mu          sync.Mutex
	store       *credentialStore
	oauth       oauthProvider
	state       savedState
	pending     *pendingLogin
	broken      bool
	closed      bool
	nextRefresh time.Time
	backoff     time.Duration
	redactions  map[string]time.Time
}

func New(directory string) (*Manager, error) {
	return newManager(directory, newOpenAI())
}

func newManager(directory string, oauth oauthProvider) (*Manager, error) {
	store, state, err := openCredentialStore(directory)
	if err != nil {
		return nil, err
	}
	m := &Manager{store: store, oauth: oauth, state: state, redactions: map[string]time.Time{}}
	if state.Version == 0 {
		state = savedState{Version: 1, HostID: "urn:uuid:" + uuid.NewString(), Generation: uuid.NewString()}
	} else if state.RefreshPending {
		// The server may have rotated the refresh token before the crash.
		state.AuthBlocked, state.RefreshPending = true, false
	}
	if err := m.save(state); err != nil {
		_ = store.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	return m.store.Close()
}

func (m *Manager) save(next savedState) error {
	if m.closed || m.broken {
		return ErrStorage
	}
	if err := m.store.save(next); err != nil {
		m.broken = true
		return ErrStorage
	}
	m.state = next
	if c := next.Credential; c != nil {
		m.redactions[c.Access], m.redactions[c.IDToken], m.redactions[c.Refresh] = time.Now().Add(24*time.Hour), time.Now().Add(24*time.Hour), time.Now().Add(31*24*time.Hour)
	}
	return nil
}

// Redact covers credentials issued after process startup as well as recent
// superseded tokens. Raw provider text is separately discarded by the runner.
func (m *Manager) Redact(value string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for secret, expiry := range m.redactions {
		if time.Now().After(expiry) {
			delete(m.redactions, secret)
			continue
		}
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Mode: "subscription", SignedIn: m.state.Credential != nil, AuthBlocked: m.state.AuthBlocked || m.state.Credential == nil, QuotaBlocked: m.state.QuotaBlocked, StorageAvailable: !m.broken && !m.closed}
	if c := m.state.Credential; c != nil {
		s.Subject, s.Email = c.Subject, c.Email
	}
	return s
}

func (m *Manager) check() error {
	if m.broken || m.closed {
		return ErrStorage
	}
	if m.state.AuthBlocked || m.state.Credential == nil {
		return ErrAuthentication
	}
	if m.state.QuotaBlocked {
		return ErrQuota
	}
	return nil
}

// Check is a side-effect-free admission check, never a model prompt.
func (m *Manager) Check() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.check()
}

func (m *Manager) Access(ctx context.Context) (AccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AccessToken{}, ErrTemporary
	}
	if err := m.check(); err != nil {
		return AccessToken{}, err
	}
	if err := m.refresh(ctx); err != nil {
		if !errors.Is(err, ErrTemporary) || !m.state.Credential.ExpiresAt.After(time.Now().Add(time.Minute)) {
			return AccessToken{}, err
		}
	}
	return AccessToken{Access: m.state.Credential.Access, ExpiresAt: m.state.Credential.ExpiresAt, Generation: m.state.Generation}, nil
}

func (m *Manager) refresh(ctx context.Context) error {
	c := *m.state.Credential
	now := time.Now()
	due := c.ExpiresAt.Add(-refreshMargin)
	if c.EarliestRefresh.After(due) {
		due = c.EarliestRefresh
	}
	if now.Before(due) && now.Before(c.ExpiresAt) {
		return nil
	}
	if now.Before(m.nextRefresh) {
		return ErrTemporary
	}
	// Persist the intent before crossing the non-transactional remote boundary.
	next := m.state
	next.RefreshPending = true
	if err := m.save(next); err != nil {
		return err
	}
	renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	renewed, err := m.oauth.renew(renewCtx, c)
	next.RefreshPending = false
	if err != nil {
		if errors.Is(err, ErrAuthentication) {
			next.AuthBlocked = true
		} else {
			m.backoff = min(max(m.backoff*2, time.Minute), 15*time.Minute)
			m.nextRefresh = now.Add(m.backoff)
		}
		if saveErr := m.save(next); saveErr != nil {
			return saveErr
		}
		if next.AuthBlocked {
			return ErrAuthentication
		}
		return ErrTemporary
	}
	next.Credential = &renewed
	if err := m.save(next); err != nil {
		return err
	}
	m.nextRefresh, m.backoff = time.Time{}, 0
	return nil
}

// Report ignores failures from credentials superseded by reauthorization.
func (m *Manager) Report(generation, failure string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if failure != QuotaFailure && failure != AuthFailure {
		return ErrLogin
	}
	if generation != m.state.Generation {
		return nil
	}
	next := m.state
	if failure == QuotaFailure {
		next.QuotaBlocked = true
	} else {
		next.AuthBlocked = true
	}
	return m.save(next)
}

func (m *Manager) Unblock() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.state
	next.QuotaBlocked = false
	next.Generation = uuid.NewString()
	return m.save(next)
}

// Logout first removes local authority durably. Remote revocation is best effort
// and cannot resurrect a removed login on failure or caller cancellation.
func (m *Manager) Logout(ctx context.Context) (revoked bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.state.Credential
	next := m.state
	next.Credential, next.AuthBlocked, next.RefreshPending, next.Generation = nil, true, false, uuid.NewString()
	if err := m.save(next); err != nil {
		return false, err
	}
	m.pending = nil
	m.nextRefresh, m.backoff = time.Time{}, 0
	if old == nil {
		return true, nil
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return m.oauth.revoke(revokeCtx, *old), nil
}

func (m *Manager) maintain(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Quota blocks inference, not maintenance of the renewable login.
	if m.closed || m.broken || m.state.Credential == nil || m.state.AuthBlocked {
		return
	}
	_ = m.refresh(ctx)
}

func (m *Manager) Run(ctx context.Context) error {
	m.maintain(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.maintain(ctx)
		}
	}
}
