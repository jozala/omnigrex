package providerauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ServeAdmin exposes operator operations only through an owner-only Unix socket.
// It is never mounted into an Execution Environment or served on the MCP port.
func (m *Manager) ServeAdmin(ctx context.Context, directory string) error {
	path := filepath.Join(directory, "admin.sock")
	if err := m.store.root.Remove("admin.sock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return ErrStorage
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return ErrStorage
	}
	server := &http.Server{Handler: http.HandlerFunc(m.serveAdmin), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 25 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return ErrStorage
	}
	return nil
}

type LoginRequest struct {
	RedirectURI string `json:"redirect_uri"`
	NewAccount  bool   `json:"new_account"`
}

type LoginCompletion struct {
	ID       string `json:"id"`
	Callback string `json:"callback"`
}

type LogoutResult struct {
	RemoteRevocationConfirmed bool `json:"remote_revocation_confirmed"`
}

func (m *Manager) serveAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Origin") != "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		_ = json.NewEncoder(w).Encode(m.Status())
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var result any = struct{}{}
	var err error
	switch r.URL.Path {
	case "/login/start":
		var input LoginRequest
		if !decodeAdmin(w, r, &input) {
			return
		}
		result, err = m.BeginLogin(input.RedirectURI, input.NewAccount)
	case "/login/complete":
		var input LoginCompletion
		if !decodeAdmin(w, r, &input) {
			return
		}
		err = m.CompleteLogin(r.Context(), input.ID, input.Callback)
	case "/logout":
		var revoked bool
		revoked, err = m.Logout(r.Context())
		result = LogoutResult{RemoteRevocationConfirmed: revoked}
	case "/unblock":
		err = m.Unblock()
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(struct {
			Error string `json:"error"`
		}{Error: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func decodeAdmin(w http.ResponseWriter, r *http.Request, result any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if d.Decode(result) != nil || d.Decode(&struct{}{}) != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

type AdminClient struct{ http *http.Client }

// Operator supports the same operations with or without the full controller.
// An offline command must acquire the same exclusive credential-writer lock.
type Operator interface {
	Status(context.Context) (Status, error)
	BeginLogin(context.Context, LoginRequest) (LoginStart, error)
	CompleteLogin(context.Context, LoginCompletion) error
	Logout(context.Context) (LogoutResult, error)
	Unblock(context.Context) error
}

func OpenOperator(ctx context.Context, directory string) (Operator, func() error, error) {
	client := NewAdminClient(directory)
	if _, err := client.Status(ctx); err == nil {
		return client, func() error { client.http.CloseIdleConnections(); return nil }, nil
	}
	manager, err := New(directory)
	if err != nil {
		return nil, nil, err
	}
	return localOperator{manager}, manager.Close, nil
}

type localOperator struct{ manager *Manager }

func (c localOperator) Status(context.Context) (Status, error) { return c.manager.Status(), nil }
func (c localOperator) BeginLogin(_ context.Context, r LoginRequest) (LoginStart, error) {
	return c.manager.BeginLogin(r.RedirectURI, r.NewAccount)
}
func (c localOperator) CompleteLogin(ctx context.Context, r LoginCompletion) error {
	return c.manager.CompleteLogin(ctx, r.ID, r.Callback)
}
func (c localOperator) Logout(ctx context.Context) (LogoutResult, error) {
	revoked, err := c.manager.Logout(ctx)
	return LogoutResult{RemoteRevocationConfirmed: revoked}, err
}
func (c localOperator) Unblock(context.Context) error { return c.manager.Unblock() }

func NewAdminClient(directory string) *AdminClient {
	path := filepath.Join(directory, "admin.sock")
	return &AdminClient{http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *AdminClient) call(ctx context.Context, method, path string, input, result any) error {
	content, err := json.Marshal(input)
	if err != nil {
		return ErrLogin
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(content))
	if err != nil {
		return ErrLogin
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("authentication manager unavailable; run the controller with subscription mode enabled")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&failure)
		if failure.Error == ErrStorage.Error() {
			return ErrStorage
		}
		return ErrLogin
	}
	if result == nil {
		return nil
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(result) != nil {
		return ErrLogin
	}
	return nil
}

func (c *AdminClient) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, http.MethodGet, "/status", nil, &s)
	return s, err
}

func (c *AdminClient) BeginLogin(ctx context.Context, input LoginRequest) (LoginStart, error) {
	var start LoginStart
	err := c.call(ctx, http.MethodPost, "/login/start", input, &start)
	return start, err
}

func (c *AdminClient) CompleteLogin(ctx context.Context, input LoginCompletion) error {
	return c.call(ctx, http.MethodPost, "/login/complete", input, nil)
}

func (c *AdminClient) Logout(ctx context.Context) (LogoutResult, error) {
	var result LogoutResult
	err := c.call(ctx, http.MethodPost, "/logout", nil, &result)
	return result, err
}

func (c *AdminClient) Unblock(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/unblock", nil, nil)
}
