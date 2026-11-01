package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type githubAPI interface {
	call(context.Context, string, string, any) ([]byte, error)
}
type httpAPI struct {
	token   string
	baseURL string
	client  *http.Client
}

func releaseHTTPClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many GitHub asset redirects")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != via[0].URL.Scheme {
			return errors.New("insecure GitHub asset redirect")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}}
}

func (a httpAPI) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			return nil, e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, a.baseURL+"/"+path, bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if strings.Contains(path, "releases/assets/") || strings.HasSuffix(path, "/zip") {
		req.Header.Set("Accept", "application/octet-stream")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := a.client.Do(req)
	if e != nil {
		return nil, errors.New("GitHub request failed (transport/redirect unavailable)")
	}
	defer resp.Body.Close()
	// Do not print raw API responses, credentials, or signed asset redirect URLs.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub %s %s returned HTTP %d", method, path, resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if len(b) > 32<<20 {
		return nil, errors.New("GitHub response exceeds size limit")
	}
	return b, e
}
func endpoint(path string) string { return "repos/" + repository + "/" + path }
func getJSON[T any](ctx context.Context, a githubAPI, path string) (T, error) {
	var result T
	b, e := a.call(ctx, "GET", path, nil)
	if e != nil {
		return result, e
	}
	e = json.Unmarshal(b, &result)
	return result, e
}
func pages[T any](ctx context.Context, a githubAPI, path, key string) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		b, e := a.call(ctx, "GET", fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), nil)
		if e != nil {
			return nil, e
		}
		if key != "" {
			var envelope map[string]json.RawMessage
			if e := json.Unmarshal(b, &envelope); e != nil {
				return nil, e
			}
			b = envelope[key]
		}
		if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return nil, errors.New("GitHub collection unavailable (null response)")
		}
		var batch []T
		if e := json.Unmarshal(b, &batch); e != nil {
			return nil, e
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

type appTagger struct {
	appID, privateKey string
	apiForToken       func(string) githubAPI
	now               func() time.Time
}

func appJWT(appID, key string, now time.Time) (string, error) {
	id, e := strconv.ParseInt(appID, 10, 64)
	if e != nil || id <= 0 || key == "" {
		return "", errors.New("configure OMNIGREX_RELEASE_APP_ID and OMNIGREX_RELEASE_APP_PRIVATE_KEY for the dedicated release App")
	}
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return "", errors.New("release App private key must be PEM RSA")
	}
	var parsed any
	if block.Type == "RSA PRIVATE KEY" {
		parsed, e = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else {
		parsed, e = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if e != nil {
		return "", errors.New("invalid release App private key")
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("release App private key must be RSA")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{appID, now.Add(-time.Minute).Unix(), now.Add(9 * time.Minute).Unix()})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, e := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	if e != nil {
		return "", errors.New("release App JWT signing failed")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Only this boundary receives the release App installation token. All other API
// calls and gh uploads retain the workflow token. No token is persisted or printed.
func (t appTagger) create(ctx context.Context, tag, sha string) (err error) {
	jwt, e := appJWT(t.appID, t.privateKey, t.now())
	if e != nil {
		return e
	}
	app := t.apiForToken(jwt)
	installation, e := getJSON[struct {
		ID int64 `json:"id"`
	}](ctx, app, endpoint("installation"))
	if e != nil {
		return fmt.Errorf("find dedicated release App installation: %w", e)
	}
	if installation.ID <= 0 {
		return errors.New("missing release App installation")
	}
	body := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{[]string{"omnigrex"}, map[string]string{"contents": "write", "workflows": "write"}}
	b, e := app.call(ctx, "POST", fmt.Sprintf("app/installations/%d/access_tokens", installation.ID), body)
	if e != nil {
		return fmt.Errorf("mint repository-scoped release token (approve App Contents/Workflows write permissions): %w", e)
	}
	var token struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	// Read the credential separately so malformed metadata still triggers revocation.
	var credential struct {
		Token string `json:"token"`
	}
	if e := json.Unmarshal(b, &credential); e != nil || credential.Token == "" {
		return errors.New("invalid or missing release App installation token")
	}
	tagAPI := t.apiForToken(credential.Token)
	// Revoke even after rejection or failed tag creation. The token also expires
	// within one hour if the runner is terminated before this cleanup can run.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, e := tagAPI.call(cleanup, "DELETE", "installation/token", nil)
		err = errors.Join(err, e)
	}()
	if e := json.Unmarshal(b, &token); e != nil {
		return errors.New("invalid release App token metadata")
	}
	if len(token.Repositories) != 1 || token.Repositories[0].FullName != repository || token.Permissions["contents"] != "write" || token.Permissions["workflows"] != "write" || token.Permissions["metadata"] != "read" || !token.ExpiresAt.After(t.now()) || token.ExpiresAt.After(t.now().Add(time.Hour+time.Minute)) {
		return errors.New("release App token scope, permissions or expiry mismatch")
	}
	for permission := range token.Permissions {
		if permission != "contents" && permission != "workflows" && permission != "metadata" {
			return errors.New("release App token has unexpected permission")
		}
	}
	_, e = tagAPI.call(ctx, "POST", endpoint("git/refs"), struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{"refs/tags/" + tag, sha})
	if e != nil {
		return fmt.Errorf("release App could not reserve exact tag; verify installation and approved Contents/Workflows write permissions: %w", e)
	}
	return nil
}
func ghUpload(ctx context.Context, workflowToken, tag string, files []string) error {
	cmd := exec.CommandContext(ctx, "gh", append([]string{"release", "upload", tag, "--repo", repository}, files...)...)
	for _, s := range os.Environ() {
		key, _, _ := strings.Cut(s, "=")
		if key != "GH_TOKEN" && key != "GITHUB_TOKEN" && key != "OMNIGREX_RELEASE_APP_PRIVATE_KEY" && key != "OMNIGREX_RELEASE_APP_ID" {
			cmd.Env = append(cmd.Env, s)
		}
	}
	cmd.Env = append(cmd.Env, "GH_TOKEN="+workflowToken)
	// Avoid echoing potentially sensitive CLI diagnostics. The failed operation
	// and draft/tag cleanup procedure are sufficient for operator investigation.
	if e := cmd.Run(); e != nil {
		return errors.New("gh release upload failed; inspect the unpublished draft and tag before retrying")
	}
	return nil
}
