package main

import (
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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPrivateKey(t *testing.T) string {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
func verifyJWT(t *testing.T, token, key string, now time.Time) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid JWT")
	}
	header, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil || string(header) != `{"alg":"RS256","typ":"JWT"}` {
		t.Fatal("unexpected JWT header")
	}
	claims, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		t.Fatal(e)
	}
	var c struct {
		Iss      string `json:"iss"`
		Iat, Exp int64
	}
	if e := json.Unmarshal(claims, &c); e != nil {
		t.Fatal(e)
	}
	if c.Iss != "123" || c.Iat != now.Add(-time.Minute).Unix() || c.Exp != now.Add(9*time.Minute).Unix() {
		t.Fatal("invalid JWT issuer or short lifetime")
	}
	block, _ := pem.Decode([]byte(key))
	rsaKey, e := x509.ParsePKCS1PrivateKey(block.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil {
		t.Fatal(e)
	}
	if e := rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, digest[:], signature); e != nil {
		t.Fatal("JWT signature invalid", e)
	}
}
func TestAppJWTKeyFormatsAndConfiguration(t *testing.T) {
	now := time.Now()
	key := testPrivateKey(t)
	token, e := appJWT("123", key, now)
	if e != nil {
		t.Fatal(e)
	}
	verifyJWT(t, token, key, now)
	block, _ := pem.Decode([]byte(key))
	rsaKey, _ := x509.ParsePKCS1PrivateKey(block.Bytes)
	b, e := x509.MarshalPKCS8PrivateKey(rsaKey)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := appJWT("123", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})), now); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ id, key string }{{"", key}, {"0", key}, {"not-id", key}, {"123", ""}, {"123", "not-pem"}, {"123", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("bad")}))}} {
		if _, e := appJWT(tc.id, tc.key, now); e == nil {
			t.Fatal("invalid App configuration accepted")
		}
	}
}
func TestAppScopePermissionExpiryAndRevocation(t *testing.T) {
	for _, kind := range []string{"wrong repository", "multiple repositories", "missing workflows", "missing contents", "extra permission", "expired", "long lived", "mint denied", "tag denied", "revoke denied"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			key := testPrivateKey(t)
			writes, revokes := 0, 0
			factory := func(token string) githubAPI {
				if token == "fixture-token" {
					return apiFunc(func(ctx context.Context, m, p string, b any) ([]byte, error) {
						if m == "DELETE" && p == "installation/token" {
							revokes++
							if kind == "revoke denied" {
								return nil, errors.New("revoke denied")
							}
							return nil, nil
						}
						writes++
						if kind == "tag denied" {
							return nil, errors.New("403 denied")
						}
						return []byte(`{}`), nil
					})
				}
				verifyJWT(t, token, key, now)
				return apiFunc(func(ctx context.Context, m, p string, b any) ([]byte, error) {
					if m == "GET" {
						return []byte(`{"id":41}`), nil
					}
					if kind == "mint denied" {
						return nil, errors.New("403 mint denied")
					}
					permissions := map[string]string{"contents": "write", "workflows": "write", "metadata": "read"}
					repos := []Repo{{repository}}
					expiry := now.Add(time.Hour)
					switch kind {
					case "wrong repository":
						repos[0].FullName = "other/repo"
					case "multiple repositories":
						repos = append(repos, Repo{"other/repo"})
					case "missing workflows":
						delete(permissions, "workflows")
					case "missing contents":
						permissions["contents"] = "read"
					case "extra permission":
						permissions["issues"] = "write"
					case "expired":
						expiry = now.Add(-time.Minute)
					case "long lived":
						expiry = now.Add(2 * time.Hour)
					}
					return mustJSON(t, map[string]any{"token": "fixture-token", "expires_at": expiry, "permissions": permissions, "repositories": repos}), nil
				})
			}
			tagger := appTagger{"123", key, factory, func() time.Time { return now }}
			e := tagger.create(context.Background(), "v0.1.0-alpha.1", strings.Repeat("a", 40))
			if e == nil {
				t.Fatal("bad authorization accepted")
			}
			if kind == "mint denied" {
				if revokes != 0 || writes != 0 {
					t.Fatal("used token after failed mint")
				}
			} else {
				if revokes != 1 {
					t.Fatal("token not revoked")
				}
				wantWrites := 0
				if kind == "tag denied" || kind == "revoke denied" {
					wantWrites = 1
				}
				if writes != wantWrites {
					t.Fatal("invalid-scope token used")
				}
			}
		})
	}
}
func TestHTTPAPIErrorsAndAssetRedirectsDoNotExposeCredentials(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential forwarded to asset download host")
		}
		_, _ = w.Write([]byte("asset"))
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer workflow-fixture" {
			t.Error("missing workflow authorization")
		}
		if strings.Contains(r.URL.Path, "assets/") {
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("asset not requested as binary")
			}
			http.Redirect(w, r, destination.URL+"/signed-secret-url", 302)
			return
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte("do not expose credential-or-raw-response"))
	}))
	defer server.Close()
	api := httpAPI{token: "workflow-fixture", baseURL: server.URL, client: releaseHTTPClient()}
	_, e := api.call(context.Background(), "POST", endpoint("git/refs"), map[string]string{"sha": "historical"})
	assertError(t, e, "HTTP 403")
	if strings.Contains(e.Error(), "workflow-fixture") || strings.Contains(e.Error(), "raw-response") {
		t.Fatal("sensitive diagnostic escaped")
	}
	b, e := api.call(context.Background(), "GET", endpoint("releases/assets/1"), nil)
	if e != nil || string(b) != "asset" {
		t.Fatalf("asset redirect: %s %v", b, e)
	}
}
func TestGHUploadUsesOnlyWorkflowCredential(t *testing.T) {
	dir := t.TempDir()
	stub := `#!/bin/sh
set -eu
printf '%s\n' "$GH_TOKEN" "${GITHUB_TOKEN-unset}" "${OMNIGREX_RELEASE_APP_PRIVATE_KEY-unset}" "${OMNIGREX_RELEASE_APP_ID-unset}" "$*" > "$CAPTURE"
`
	if e := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0700); e != nil {
		t.Fatal(e)
	}
	capture := filepath.Join(dir, "capture")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("CAPTURE", capture)
	t.Setenv("GH_TOKEN", "wrong")
	t.Setenv("GITHUB_TOKEN", "wrong")
	t.Setenv("OMNIGREX_RELEASE_APP_PRIVATE_KEY", "private-fixture")
	t.Setenv("OMNIGREX_RELEASE_APP_ID", "123")
	if e := ghUpload(context.Background(), "workflow-fixture", "v0.1.0-alpha.1", []string{"payload.tar.gz"}); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(capture)
	if e != nil {
		t.Fatal(e)
	}
	want := "workflow-fixture\nunset\nunset\nunset\nrelease upload v0.1.0-alpha.1 --repo jozala/omnigrex payload.tar.gz\n"
	if string(b) != want {
		t.Fatalf("unexpected gh credential/operation routing: %s", b)
	}
}
func TestCommandFailsClosedBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown", "out"}} {
		if e := execute(context.Background(), args); e == nil {
			t.Fatal("invalid command accepted")
		}
	}
	for _, name := range []string{"GH_TOKEN", "OMNIGREX_RELEASE_APP_ID", "OMNIGREX_RELEASE_APP_PRIVATE_KEY"} {
		t.Setenv(name, "")
	}
	assertError(t, execute(context.Background(), []string{"alpha", "out"}), "GH_TOKEN")
	t.Setenv("GH_TOKEN", "workflow-fixture")
	t.Setenv("GITHUB_REPOSITORY", repository)
	t.Setenv("GITHUB_REF", "refs/heads/other")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	assertError(t, execute(context.Background(), []string{"alpha", "out"}), "main")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_EVENT_NAME", "push")
	assertError(t, execute(context.Background(), []string{"stable", "out"}), "manually")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	assertError(t, execute(context.Background(), []string{"alpha", "out"}), "configure OMNIGREX_RELEASE_APP_ID")
}

func TestPaginationAndMissingEvidenceMetadata(t *testing.T) {
	for _, key := range []string{"", "jobs"} {
		calls := 0
		api := apiFunc(func(ctx context.Context, m, p string, b any) ([]byte, error) {
			calls++
			if !strings.HasSuffix(p, fmt.Sprintf("page=%d", calls)) {
				t.Fatal("pagination did not advance")
			}
			values := make([]Tag, 100)
			if calls == 2 {
				values = []Tag{{Name: "last"}}
			}
			if key != "" {
				return mustJSON(t, map[string]any{key: values}), nil
			}
			return mustJSON(t, values), nil
		})
		all, e := pages[Tag](context.Background(), api, "fixture", key)
		if e != nil || len(all) != 101 || all[100].Name != "last" || calls != 2 {
			t.Fatalf("pagination: %d %d %v", len(all), calls, e)
		}
	}
	for _, data := range []string{`{"id":1,"name":"candidate"}`, `{"id":1,"name":"candidate","expired":null}`} {
		var a Artifact
		if e := json.Unmarshal([]byte(data), &a); e == nil {
			t.Fatal("missing artifact expiry accepted")
		}
	}
	for _, data := range []string{`{"tag_name":"v0.1.0-alpha.1","prerelease":true}`, `{"tag_name":"v0.1.0-alpha.1","draft":null,"prerelease":true}`} {
		var r Release
		if e := json.Unmarshal([]byte(data), &r); e == nil {
			t.Fatal("unavailable publication state accepted")
		}
	}
}

func TestUnavailableCollectionsAreNotEmptySuccess(t *testing.T) {
	for _, data := range []string{"null", `{"jobs":null}`, `{"other":[]}`} {
		key := "jobs"
		if data == "null" {
			key = ""
		}
		api := apiFunc(func(context.Context, string, string, any) ([]byte, error) { return []byte(data), nil })
		if _, e := pages[Job](context.Background(), api, "fixture", key); e == nil {
			t.Fatal("unavailable collection accepted as empty")
		}
	}
}
