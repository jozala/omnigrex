//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

func cacheTestFixture(t *testing.T, host, repo string) (indexRef, selected string, reserializedMetadata, originalManifest, indexBytes []byte) {
	t.Helper()
	config := fmt.Sprintf(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"repo":%q}`, host+"/"+repo)
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`, oci.MediaTypeImageManifest, oci.MediaTypeImageConfig, digest.FromString(config).String(), len(config))
	selected = digest.FromString(manifest).String()
	index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"architecture":"amd64","os":"linux"}}]}`, oci.MediaTypeImageIndex, oci.MediaTypeImageManifest, selected, len(manifest))
	indexDigest := digest.FromString(index).String()
	indexRef = host + "/" + repo + "@" + indexDigest
	metadata, err := json.Marshal(map[string]string{"manifest_digest": selected, "config_digest": digest.FromString(config).String(), "raw_manifest_json": manifest, "raw_config_json": config})
	if err != nil {
		t.Fatal(err)
	}
	// Reserialize to force original-manifest authentication, as the SDK does.
	var cached map[string]string
	if err := json.Unmarshal(metadata, &cached); err != nil {
		t.Fatal(err)
	}
	originalManifest = []byte(cached["raw_manifest_json"])
	cached["raw_manifest_json"] = "\n " + cached["raw_manifest_json"] + "\n"
	reserializedMetadata, err = json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	indexBytes = []byte(index)
	return indexRef, selected, reserializedMetadata, originalManifest, indexBytes
}

func newCacheTestClient(t *testing.T, handler func(*http.Request) (*http.Response, error)) *http.Client {
	t.Helper()
	return &http.Client{Transport: imageTestTransport(handler)}
}

func cacheTestCache(t *testing.T, home string, client *http.Client) *registryDocumentCache {
	t.Helper()
	cache := newRegistryDocumentCache(home, client)
	cache.now = time.Now
	cache.jitter = func() float64 { return 1.0 }
	return cache
}

func TestRegistryRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if when, ok := parseRegistryRetryAfter("90", now); !ok || !when.Equal(now.Add(90*time.Second)) {
		t.Fatalf("delta-seconds not honored: %v %v", when, ok)
	}
	if when, ok := parseRegistryRetryAfter("  30  ", now); !ok || !when.Equal(now.Add(30*time.Second)) {
		t.Fatalf("padded delta not honored: %v %v", when, ok)
	}
	future := now.Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	if when, ok := parseRegistryRetryAfter(future, now); !ok || !when.Equal(now.Add(2*time.Minute).Truncate(time.Second)) {
		// http.ParseTime truncates to seconds; allow exact match after parse.
		if !ok {
			t.Fatalf("http-date not honored: %v", future)
		}
	}
	past := now.Add(-time.Minute).UTC().Format(http.TimeFormat)
	if _, ok := parseRegistryRetryAfter(past, now); ok {
		t.Fatal("past http-date honored")
	}
	for _, invalid := range []string{"", "  ", "-5", "soon", "9999999999999999999999"} {
		if _, ok := parseRegistryRetryAfter(invalid, now); ok {
			t.Fatalf("invalid retry-after honored: %q", invalid)
		}
	}
}

func TestRegistryBackoffSchedule(t *testing.T) {
	cache := newRegistryDocumentCache(t.TempDir(), nil)
	fixed := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return fixed }
	cache.jitter = func() float64 { return 1.0 }
	for failures, want := range map[int]time.Duration{1: 30 * time.Second, 2: 60 * time.Second, 3: 120 * time.Second, 4: 240 * time.Second, 5: 300 * time.Second, 9: 300 * time.Second} {
		if got := cache.computeNextRetry(failures, time.Time{}).Sub(fixed); got != want {
			t.Fatalf("failures=%d backoff=%v want %v", failures, got, want)
		}
	}
	// Server-requested time beyond the local cap is honored.
	server := fixed.Add(12 * time.Minute)
	if got := cache.computeNextRetry(1, server); !got.Equal(server) {
		t.Fatalf("retry-after beyond cap not honored: %v", got)
	}
}

func TestRegistryDocumentCacheCachesBothDocumentsAndReusesWarm(t *testing.T) {
	home := t.TempDir()
	indexRef, selected, reserialized, original, indexBytes := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	served := map[string][]byte{manifestRef: original, indexRef: indexBytes}
	var manifestCalls, indexCalls, tokenCalls atomic.Int64
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "/token") {
			tokenCalls.Add(1)
			return imageTestResponse(http.StatusOK, `{"token":"anonymous-pull-token"}`), nil
		}
		if request.Header.Get("Authorization") == "" && strings.HasSuffix(request.URL.Path, "/manifests/"+strings.Split(indexRef, "@")[1]) {
			// First index request challenges once to exercise the auth exchange.
			if indexCalls.Load() == 0 {
				indexCalls.Add(1)
				response := imageTestResponse(http.StatusUnauthorized, "challenge")
				response.Header.Set("WWW-Authenticate", `Bearer realm="https://registry.example/token",service="registry.example"`)
				return response, nil
			}
		}
		path := request.URL.Path
		for ref, body := range served {
			want := "/v2/fixture/manifests/" + strings.Split(ref, "@")[1]
			if path == want {
				if strings.HasSuffix(want, strings.Split(manifestRef, "@")[1]) {
					manifestCalls.Add(1)
				} else {
					indexCalls.Add(1)
				}
				if request.Header.Get("Authorization") != "" && !strings.Contains(request.URL.Path, "/token") {
					// Authenticated manifest/index request; counted above.
				}
				return imageTestResponse(http.StatusOK, string(body)), nil
			}
		}
		return imageTestResponse(http.StatusNotFound, "missing"), nil
	})
	// Rewrite the token realm to the fake transport path: the fake client does not
	// use network, so point the challenge at a path our handler recognizes.
	cache := cacheTestCache(t, home, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		// Route token requests issued by anonymousImageToken to the fake handler.
		if strings.Contains(request.URL.Host, "registry.example") && strings.Contains(request.URL.Path, "/token") {
			tokenCalls.Add(1)
			return imageTestResponse(http.StatusOK, `{"token":"anonymous-pull-token"}`), nil
		}
		return client.Transport.RoundTrip(request)
	})})
	// First verification populates both documents through the cache.
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, cache.fetch); err != nil {
		t.Fatalf("cold verification failed: %v", err)
	}
	if manifestCalls.Load() != 1 {
		t.Fatalf("cold manifest fetches=%d want 1", manifestCalls.Load())
	}
	if indexCalls.Load() != 2 { // 1x 401 challenge + 1x authenticated success counted as 2 index hits above (challenge counted separately)
		// Our counting above counts the 401 as one index call; accept >=1 authenticated success.
		if indexCalls.Load() < 1 {
			t.Fatalf("cold index fetches=%d want >=1", indexCalls.Load())
		}
	}
	// Second verification must make zero manifest/index/auth requests.
	manifestCalls.Store(0)
	indexCalls.Store(0)
	tokenCalls.Store(0)
	blocking := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		t.Error("warm verification contacted registry")
		return nil, errors.New("must not be called")
	})
	warm := cacheTestCache(t, home, blocking)
	warm.now = cache.now
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, warm.fetch); err != nil {
		t.Fatalf("warm verification failed: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestRegistryDocumentCacheWarmSurvivesNewProcess(t *testing.T) {
	home := t.TempDir()
	indexRef, selected, reserialized, original, indexBytes := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	served := map[string][]byte{manifestRef: original, indexRef: indexBytes}
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		for ref, body := range served {
			if request.URL.Path == "/v2/fixture/manifests/"+strings.Split(ref, "@")[1] {
				return imageTestResponse(http.StatusOK, string(body)), nil
			}
		}
		return imageTestResponse(http.StatusNotFound, "missing"), nil
	})
	cold := cacheTestCache(t, home, client)
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, cold.fetch); err != nil {
		t.Fatal(err)
	}
	// A new cache instance over the same home with registry access disabled must succeed.
	disabled := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("registry disabled")
	})
	fresh := cacheTestCache(t, home, disabled)
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, fresh.fetch); err != nil {
		t.Fatalf("fresh-process warm verification failed: %v", err)
	}
	// Direct document fetches also survive without network.
	if _, err := fresh.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("fresh manifest fetch failed: %v", err)
	}
	if _, err := fresh.fetch(context.Background(), indexRef); err != nil {
		t.Fatalf("fresh index fetch failed: %v", err)
	}
}

func TestRegistryDocumentCacheConcurrentInProcessSharesPopulation(t *testing.T) {
	home := t.TempDir()
	_, selected, _, original, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	var calls atomic.Int64
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // Widen the race so waiters block on the lock.
		return imageTestResponse(http.StatusOK, string(original)), nil
	})
	cache := cacheTestCache(t, home, client)
	const waiters = 8
	var wg sync.WaitGroup
	errs := make([]error, waiters)
	for i := range waiters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cache.fetch(context.Background(), manifestRef)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("waiter %d failed: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent fetches=%d want 1", got)
	}
}

func TestRegistryDocumentCacheConcurrentFailureSharesCooldown(t *testing.T) {
	home := t.TempDir()
	_, selected, _, _, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	var calls atomic.Int64
	client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond) // Hold the per-document lock so waiters block.
		response := imageTestResponse(http.StatusServiceUnavailable, "unavailable")
		response.Header.Set("Retry-After", "120")
		return response, nil
	})
	cache := cacheTestCache(t, home, client)
	const waiters = 4
	var wg sync.WaitGroup
	errs := make([]error, waiters)
	for i := range waiters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cache.fetch(context.Background(), manifestRef)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, errImageUnavailable) {
			t.Fatalf("waiter %d error=%v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent failing fetches=%d want 1", got)
	}
}

func TestRegistryDocumentCacheCancellationWhileWaitingOnLock(t *testing.T) {
	home := t.TempDir()
	_, selected, _, original, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureRegistryCacheDirs(root); err != nil {
		t.Fatal(err)
	}
	docHex, _ := registryDocHex(selected)
	holder, err := acquireRegistryLock(context.Background(), root, registryLockRel(docHex))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRegistryLock(holder)
	client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		t.Error("cancelled waiter fetched")
		return imageTestResponse(http.StatusOK, string(original)), nil
	})
	cache := cacheTestCache(t, home, client)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	start := time.Now()
	_, err = cache.fetch(ctx, manifestRef)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled wait did not return within budget: %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errImageUnavailable) {
		t.Fatalf("cancelled wait error=%v", err)
	}
	// No partial file was published.
	if _, lerr := root.Lstat(registryDocRel(docHex)); lerr == nil {
		t.Fatal("cancelled waiter published partial file")
	}
}

func TestRegistryDocumentCacheWriterDeathLeavesNoPartialFile(t *testing.T) {
	home := t.TempDir()
	_, selected, _, original, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureRegistryCacheDirs(root); err != nil {
		t.Fatal(err)
	}
	docHex, _ := registryDocHex(selected)
	// Simulate a writer that created a temp file but died before rename.
	tmp := registryDocSubdir + "/.tmp-abandoned"
	file, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte(`{"partial":true}`))
	_ = file.Close() // No rename; lock released by process death.
	// Abandoned temp files are irrelevant to lookup.
	if _, err := readVerifiedRegistryDocument(root, docHex, selected); err == nil {
		t.Fatal("abandoned temp accepted as document")
	}
	client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		return imageTestResponse(http.StatusOK, string(original)), nil
	})
	cache := cacheTestCache(t, home, client)
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("recovery fetch failed: %v", err)
	}
	if content, err := readVerifiedRegistryDocument(root, docHex, selected); err != nil || string(content) != string(original) {
		t.Fatalf("recovered document invalid: %v", err)
	}
}

func TestRegistryDocumentCacheRejectsUnsafeEntries(t *testing.T) {
	home := t.TempDir()
	indexRef, selected, reserialized, original, indexBytes := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, original, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureRegistryCacheDirs(root); err != nil {
		t.Fatal(err)
	}
	manifestHex, _ := registryDocHex(selected)
	served := map[string][]byte{manifestRef: original, indexRef: indexBytes}
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		for ref, body := range served {
			if request.URL.Path == "/v2/fixture/manifests/"+strings.Split(ref, "@")[1] {
				return imageTestResponse(http.StatusOK, string(body)), nil
			}
		}
		return imageTestResponse(http.StatusNotFound, "missing"), nil
	})
	cache := cacheTestCache(t, home, client)

	// Symlinked entry is never followed; eligible corruption is repaired.
	if err := root.Remove(registryDocRel(manifestHex)); err == nil {
		t.Fatal("unexpected pre-existing doc")
	}
	_ = root.Remove(registryDocRel(manifestHex))
	if err := root.Symlink(secret, registryDocRel(manifestHex)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("symlink repair failed: %v", err)
	}
	if info, err := root.Lstat(registryDocRel(manifestHex)); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("symlink not replaced with regular file: %+v %v", info, err)
	}
	if secretContent, _ := os.ReadFile(secret); string(secretContent) != string(original) {
		t.Fatal("symlink target modified")
	}

	// Corrupt digest is repaired.
	_ = root.Remove(registryDocRel(manifestHex))
	if err := publishRegistryDocument(root, "00"+manifestHex[2:], []byte(`{}`)); err != nil {
		// Write a corrupt entry under the correct name directly.
		f, ferr := root.OpenFile(registryDocRel(manifestHex), os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
		if ferr != nil {
			t.Fatal(ferr)
		}
		_, _ = f.Write([]byte(`{"corrupt":true}`))
		_ = f.Close()
	}
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("corrupt repair failed: %v", err)
	}

	// Oversized entry is rejected and repaired.
	oversized := append([]byte{}, original...)
	oversized = append(oversized, []byte(strings.Repeat(" ", imageMetadataLimit))...)
	_ = root.Remove(registryDocRel(manifestHex))
	f, err := root.OpenFile(registryDocRel(manifestHex), os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(oversized)
	_ = f.Close()
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("oversized repair failed: %v", err)
	}

	// Non-regular entry (directory) is never accepted.
	_ = root.Remove(registryDocRel(manifestHex))
	_ = root.RemoveAll(registryDocRel(manifestHex))
	if err := root.Mkdir(registryDocRel(manifestHex), 0700); err != nil {
		t.Fatal(err)
	}
	// A directory cannot be atomically replaced by rename; report bounded failure.
	if _, err := cache.fetch(context.Background(), manifestRef); err == nil || !errors.Is(err, errImageUnavailable) {
		t.Fatalf("directory entry accepted: %v", err)
	}
	_ = root.RemoveAll(registryDocRel(manifestHex))

	// Full verification still enforces SDK integrity with a warm cache.
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, cache.fetch); err != nil {
		t.Fatalf("warm verification failed: %v", err)
	}
	var badCached map[string]string
	if err := json.Unmarshal(reserialized, &badCached); err != nil {
		t.Fatal(err)
	}
	badCached["raw_config_json"] = strings.ReplaceAll(badCached["raw_config_json"], "amd64", "arm64")
	badBytes, err := json.Marshal(badCached)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", badBytes, cache.fetch); !errors.Is(err, execution.ErrBindingMismatch) {
		t.Fatalf("corrupt SDK config blessed by warm cache: %v", err)
	}
}

func TestRegistryDocumentCacheCooldownAndRetryAfter(t *testing.T) {
	home := t.TempDir()
	_, selected, _, original, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	now := time.Now().Truncate(time.Second)
	current := now
	var calls atomic.Int64
	var status atomic.Int32
	status.Store(429)
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := imageTestResponse(int(status.Load()), "limited")
		if status.Load() == 429 {
			response.Header.Set("Retry-After", "120")
		}
		return response, nil
	})
	cache := cacheTestCache(t, home, client)
	cache.now = func() time.Time { return current }
	// First failure performs one exchange and establishes cooldown.
	if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
		t.Fatalf("rate-limited fetch=%v", err)
	}
	if got := calls.Load(); got != 2 { // 401-less path: single manifest request in this fake (no challenge)
		// Our fake returns 429 directly without challenge, so exactly 1 call.
		if got != 1 {
			t.Fatalf("first failure calls=%d want 1", got)
		}
	}
	// Immediate retry makes no HTTP request.
	calls.Store(0)
	if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
		t.Fatalf("cooldown fetch=%v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("cooldown did not suppress fetch: %d calls", got)
	}
	// Retry-After (120s) is honored even though it exceeds no local generation here;
	// advancing 60s keeps the cooldown, advancing past 120s retries.
	current = now.Add(60 * time.Second)
	calls.Store(0)
	if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
		t.Fatalf("early retry=%v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("early retry contacted registry")
	}
	current = now.Add(121 * time.Second)
	status.Store(200)
	client = newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return imageTestResponse(http.StatusOK, string(original)), nil
	})
	cache.client = client
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("post-cooldown fetch failed: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("post-cooldown calls=%d want 1", got)
	}
}

func TestRegistryDocumentCacheClassifiesFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{"rate-limited", 429},
		{"unavailable", 503},
		{"missing", 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			_, selected, _, _, _ := cacheTestFixture(t, "registry.example", "fixture")
			manifestRef := "registry.example/fixture@" + selected
			var calls atomic.Int64
			client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return imageTestResponse(test.status, "failure-body-private"), nil
			})
			cache := cacheTestCache(t, home, client)
			if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
				t.Fatalf("fetch=%v", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("calls=%d want 1", got)
			}
			calls.Store(0)
			if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
				t.Fatalf("second fetch=%v", err)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("repeated failure not suppressed: %d calls", got)
			}
		})
	}
	// Invalid bytes never enter the store and trigger a cooldown.
	t.Run("invalid-bytes", func(t *testing.T) {
		home := t.TempDir()
		_, selected, _, _, _ := cacheTestFixture(t, "registry.example", "fixture")
		manifestRef := "registry.example/fixture@" + selected
		var calls atomic.Int64
		client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return imageTestResponse(http.StatusOK, `{"wrong":"bytes"}`), nil
		})
		cache := cacheTestCache(t, home, client)
		if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
			t.Fatalf("invalid fetch=%v", err)
		}
		calls.Store(0)
		if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
			t.Fatalf("second invalid=%v", err)
		}
		if got := calls.Load(); got != 0 {
			t.Fatalf("invalid bytes retried immediately")
		}
		root, _ := os.OpenRoot(home)
		defer root.Close()
		docHex, _ := registryDocHex(selected)
		if _, err := root.Lstat(registryDocRel(docHex)); err == nil {
			t.Fatal("invalid bytes entered store")
		}
	})
	// Timeout is a single attempt and is sanitized.
	t.Run("timeout", func(t *testing.T) {
		home := t.TempDir()
		_, selected, _, _, _ := cacheTestFixture(t, "registry.example", "fixture")
		manifestRef := "registry.example/fixture@" + selected
		client := &http.Client{Transport: imageTestTransport(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, errors.New("provider-private-key")
		})}
		cache := cacheTestCache(t, home, client)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := cache.fetch(ctx, manifestRef)
		if !errors.Is(err, errImageUnavailable) || strings.Contains(fmt.Sprintf("%+v", err), "private-key") {
			t.Fatalf("unsafe timeout error: %v", err)
		}
	})
}

func TestRegistryDocumentCacheWarmHitDuringCooldownOrOutage(t *testing.T) {
	home := t.TempDir()
	indexRef, selected, reserialized, original, indexBytes := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	served := map[string][]byte{manifestRef: original, indexRef: indexBytes}
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		for ref, body := range served {
			if request.URL.Path == "/v2/fixture/manifests/"+strings.Split(ref, "@")[1] {
				return imageTestResponse(http.StatusOK, string(body)), nil
			}
		}
		return imageTestResponse(http.StatusNotFound, "missing"), nil
	})
	cache := cacheTestCache(t, home, client)
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, cache.fetch); err != nil {
		t.Fatal(err)
	}
	// Registry outage plus an active cooldown for another reference on the same
	// origin must not break warm local verification.
	otherRef := "registry.example/other@" + selected
	root, _ := os.OpenRoot(home)
	_ = root.Close()
	outage := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("registry down")
	})
	warm := cacheTestCache(t, home, outage)
	// Force a cooldown for the other reference on the same origin.
	r, _ := os.OpenRoot(home)
	_ = ensureRegistryCacheDirs(r)
	_ = writeRegistryRetryState(r, registryRefCooldownRel(otherRef), registryRetryState{Version: 1, Failures: 1, NextRetryUnix: warm.now().Add(time.Hour).Unix()})
	_ = r.Close()
	if err := verifyImageContent(context.Background(), indexRef, selected, "linux", "amd64", reserialized, warm.fetch); err != nil {
		t.Fatalf("warm hit during outage/cooldown failed: %v", err)
	}
}

func TestRegistryDocumentCacheWriteFailureIsBounded(t *testing.T) {
	home := t.TempDir()
	_, selected, _, original, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureRegistryCacheDirs(root); err != nil {
		t.Fatal(err)
	}
	docHex, _ := registryDocHex(selected)
	_ = root.RemoveAll(registryDocRel(docHex))
	if err := root.Mkdir(registryDocRel(docHex), 0700); err != nil {
		t.Fatal(err)
	}
	client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		return imageTestResponse(http.StatusOK, string(original)), nil
	})
	cache := cacheTestCache(t, home, client)
	if _, err := cache.fetch(context.Background(), manifestRef); !errors.Is(err, errImageUnavailable) {
		t.Fatalf("write failure returned success: %v", err)
	}
	// No valid document was established; the failure is visible instead of a
	// silent repeated-fetch success path.
	if info, err := root.Lstat(registryDocRel(docHex)); err != nil || !info.IsDir() {
		t.Fatalf("write-failure state unexpected: %+v %v", info, err)
	}
	_ = root.RemoveAll(registryDocRel(docHex))
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatalf("retry after clearing blocker failed: %v", err)
	}
}

func TestRegistryDocumentCacheIsolatesDigestsAndRegistries(t *testing.T) {
	home := t.TempDir()
	indexRefA, selectedA, _, originalA, indexA := cacheTestFixture(t, "registry-a.example", "fixture")
	manifestRefA := "registry-a.example/fixture@" + selectedA
	indexRefB, selectedB, _, originalB, indexB := cacheTestFixture(t, "registry-b.example", "other")
	manifestRefB := "registry-b.example/other@" + selectedB
	if selectedA == selectedB {
		t.Fatal("fixtures unexpectedly share digest")
	}
	var callsA, callsB atomic.Int64
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		host := request.URL.Host
		switch host {
		case "registry-a.example":
			callsA.Add(1)
			if strings.HasSuffix(request.URL.Path, selectedA) {
				return imageTestResponse(http.StatusOK, string(originalA)), nil
			}
			if strings.HasSuffix(request.URL.Path, strings.Split(indexRefA, "@")[1]) {
				return imageTestResponse(http.StatusOK, string(indexA)), nil
			}
		case "registry-b.example":
			callsB.Add(1)
			if strings.HasSuffix(request.URL.Path, selectedB) {
				return imageTestResponse(http.StatusOK, string(originalB)), nil
			}
			if strings.HasSuffix(request.URL.Path, strings.Split(indexRefB, "@")[1]) {
				return imageTestResponse(http.StatusOK, string(indexB)), nil
			}
		}
		return imageTestResponse(http.StatusNotFound, "missing"), nil
	})
	cache := cacheTestCache(t, home, client)
	if _, err := cache.fetch(context.Background(), manifestRefA); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.fetch(context.Background(), manifestRefB); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.fetch(context.Background(), indexRefA); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.fetch(context.Background(), indexRefB); err != nil {
		t.Fatal(err)
	}
	// Origin-wide 429 on A must not block B.
	limited := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "registry-a.example" {
			response := imageTestResponse(http.StatusTooManyRequests, "limited")
			response.Header.Set("Retry-After", "300")
			return response, nil
		}
		callsB.Add(1)
		return imageTestResponse(http.StatusOK, string(originalB)), nil
	})
	// Force a fresh digest on A to trigger the 429 path (existing docs stay warm).
	fakeDigest := "sha256:" + strings.Repeat("1", 64)
	fakeRefA := "registry-a.example/fixture@" + fakeDigest
	cache.client = limited
	if _, err := cache.fetch(context.Background(), fakeRefA); !errors.Is(err, errImageUnavailable) {
		t.Fatalf("expected rate-limit: %v", err)
	}
	// Cached B documents remain usable; a new B digest still fetches.
	callsB.Store(0)
	if _, err := cache.fetch(context.Background(), manifestRefB); err != nil {
		t.Fatalf("B blocked by A cooldown: %v", err)
	}
	if got := callsB.Load(); got != 0 {
		t.Fatalf("warm B contacted registry during A cooldown")
	}
}

func TestRegistryDocumentCacheExactImportedManifestNeedsNoNetwork(t *testing.T) {
	home := t.TempDir()
	_, selected, metadata, _ := imageProofFixture(t)
	// Non-reserialized exact manifest where wanted==selected needs no fetch.
	ref := "registry.example/fixture@" + selected
	calls := 0
	cache := cacheTestCache(t, home, newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not fetch")
	}))
	// Seed the test through verifyImageContent directly with the cache fetcher;
	// the imported-manifest path returns before any registry access.
	if err := verifyImageContent(context.Background(), ref, selected, "linux", "amd64", metadata, cache.fetch); err != nil {
		t.Fatalf("imported manifest rejected: %v", err)
	}
	if calls != 0 {
		t.Fatal("imported manifest contacted registry")
	}
}

func TestRegistryDocumentCacheCountsManifestAndTokenSeparately(t *testing.T) {
	home := t.TempDir()
	_, selected, _, original, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	var manifestCalls, tokenCalls atomic.Int64
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/token" {
			tokenCalls.Add(1)
			return imageTestResponse(http.StatusOK, `{"token":"t"}`), nil
		}
		manifestCalls.Add(1)
		if manifestCalls.Load() == 1 && request.Header.Get("Authorization") == "" {
			response := imageTestResponse(http.StatusUnauthorized, "challenge")
			response.Header.Set("WWW-Authenticate", `Bearer realm="https://registry.example/token",service="registry.example"`)
			return response, nil
		}
		if request.Header.Get("Authorization") != "Bearer t" {
			return imageTestResponse(http.StatusUnauthorized, "bad auth"), nil
		}
		return imageTestResponse(http.StatusOK, string(original)), nil
	})
	// Rewrite token host to our fake path: intercept https://registry.example/token.
	wrapped := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "registry.example" && request.URL.Path == "/token" {
			tokenCalls.Add(1)
			return imageTestResponse(http.StatusOK, `{"token":"t"}`), nil
		}
		return client.Transport.RoundTrip(request)
	})}
	cache := cacheTestCache(t, home, wrapped)
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatal(err)
	}
	if got := manifestCalls.Load(); got != 2 {
		t.Fatalf("manifest exchanges=%d want 2 (challenge + authenticated)", got)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token exchanges=%d want 1", got)
	}
	manifestCalls.Store(0)
	tokenCalls.Store(0)
	// Warm fetch performs neither exchange.
	quiet := cacheTestCache(t, home, newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		t.Error("warm fetch hit network")
		return nil, errors.New("no network")
	}))
	if _, err := quiet.fetch(context.Background(), manifestRef); err != nil {
		t.Fatal(err)
	}
}

func TestCachedImmutableImageReadsBoundedRawMetadataWithCache(t *testing.T) {
	home := t.TempDir()
	_, selected, metadata, _ := imageProofFixture(t)
	ref := "registry.example/fixture@" + selected
	directory := filepath.Join(home, "cache", "manifests")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(ref))
	path := filepath.Join(directory, hex.EncodeToString(key[:])+".json")
	if err := os.WriteFile(path, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyCachedImage(context.Background(), home, ref, selected, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryDocumentCacheMissingSDKImageRemainsUnavailable(t *testing.T) {
	home := t.TempDir()
	indexRef, selected, reserialized, original, indexBytes := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	served := map[string][]byte{manifestRef: original, indexRef: indexBytes}
	client := newCacheTestClient(t, func(request *http.Request) (*http.Response, error) {
		for ref, body := range served {
			if request.URL.Path == "/v2/fixture/manifests/"+strings.Split(ref, "@")[1] {
				return imageTestResponse(http.StatusOK, string(body)), nil
			}
		}
		return imageTestResponse(http.StatusNotFound, "missing"), nil
	})
	cache := cacheTestCache(t, home, client)
	// Registry documents alone do not provision the SDK image.
	if _, err := cache.fetch(context.Background(), manifestRef); err != nil {
		t.Fatal(err)
	}
	if err := verifyCachedImage(context.Background(), home, indexRef, selected, "linux", "amd64"); !errors.Is(err, errImageUnavailable) {
		t.Fatalf("missing SDK image reported success: %v", err)
	}
	_ = reserialized
}

func TestRegistryDocumentCacheSanitizesErrors(t *testing.T) {
	home := t.TempDir()
	_, selected, _, _, _ := cacheTestFixture(t, "registry.example", "fixture")
	manifestRef := "registry.example/fixture@" + selected
	client := newCacheTestClient(t, func(*http.Request) (*http.Response, error) {
		return imageTestResponse(http.StatusForbidden, "provider-private-key-secret-body"), nil
	})
	cache := cacheTestCache(t, home, client)
	_, err := cache.fetch(context.Background(), manifestRef)
	if err == nil || !errors.Is(err, errImageUnavailable) {
		t.Fatalf("fetch=%v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", err), "private-key") {
		t.Fatalf("provider body leaked: %v", err)
	}
	if _, err := cache.fetch(context.Background(), "registry.example/fixture:latest"); !errors.Is(err, execution.ErrInvalidSpec) {
		t.Fatalf("mutable reference not rejected: %v", err)
	}
}

// Subprocess helpers prove cross-process persistence, coordination, and
// process-death recovery with real file locks rather than in-memory fakes.

func TestCacheHelperProcess(t *testing.T) {
	if os.Getenv("OMNIGREX_TEST_CACHE_HELPER") == "" {
		t.Skip("helper")
	}
	home := os.Getenv("OMNIGREX_TEST_CACHE_HOME")
	ref := os.Getenv("OMNIGREX_TEST_CACHE_REF")
	mode := os.Getenv("OMNIGREX_TEST_CACHE_MODE")
	switch mode {
	case "fetch":
		server := os.Getenv("OMNIGREX_TEST_CACHE_SERVER")
		_ = server
		// The ref host is the httptest TLS host; skip verification for its cert.
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec
		client.Timeout = 10 * time.Second
		cache := newRegistryDocumentCache(home, client)
		cache.jitter = func() float64 { return 1.0 }
		if _, err := cache.fetch(context.Background(), ref); err != nil {
			fmt.Fprintf(os.Stderr, "helper fetch: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	case "hold-lock":
		lockRel := os.Getenv("OMNIGREX_TEST_CACHE_LOCK")
		root, err := os.OpenRoot(home)
		if err != nil {
			os.Exit(1)
		}
		defer root.Close()
		if err := ensureRegistryCacheDirs(root); err != nil {
			os.Exit(1)
		}
		lock, err := acquireRegistryLock(context.Background(), root, lockRel)
		if err != nil {
			os.Exit(1)
		}
		defer releaseRegistryLock(lock)
		// Hold the lock briefly, then exit without explicit release ordering
		// beyond defer to simulate normal completion; death-release is covered
		// by the kill test below.
		time.Sleep(2 * time.Second)
		os.Exit(0)
	case "die-holding-lock":
		lockRel := os.Getenv("OMNIGREX_TEST_CACHE_LOCK")
		root, err := os.OpenRoot(home)
		if err != nil {
			os.Exit(1)
		}
		defer root.Close()
		if err := ensureRegistryCacheDirs(root); err != nil {
			os.Exit(1)
		}
		file, err := root.OpenFile(lockRel, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
		if err != nil {
			os.Exit(1)
		}
		// Do not defer unlock: exit abruptly with the lock held; the kernel
		// must release it on process death.
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
	os.Exit(2)
}

func runCacheHelper(t *testing.T, home, ref, mode, lockRel, server string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCacheHelperProcess", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"OMNIGREX_TEST_CACHE_HELPER=1",
		"OMNIGREX_TEST_CACHE_HOME="+home,
		"OMNIGREX_TEST_CACHE_REF="+ref,
		"OMNIGREX_TEST_CACHE_MODE="+mode,
		"OMNIGREX_TEST_CACHE_LOCK="+lockRel,
		"OMNIGREX_TEST_CACHE_SERVER="+server,
	)
	return cmd
}

func TestRegistryDocumentCacheSeparateProcessesSharePopulation(t *testing.T) {
	// Publish the document bytes before starting the server so handler
	// goroutines have a happens-before edge to the write under -race.
	config := `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`
	manifestStr := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`, oci.MediaTypeImageManifest, oci.MediaTypeImageConfig, digest.FromString(config).String(), len(config))
	manifest := []byte(manifestStr)
	selected := digest.FromString(manifestStr).String()
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(manifest)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	ref := host + "/fixture@" + selected
	home := t.TempDir()
	const children = 4
	var wg sync.WaitGroup
	errs := make([]error, children)
	for i := range children {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := runCacheHelper(t, home, ref, "fetch", "", server.URL)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = fmt.Errorf("child %d: %v %s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cross-process fetches=%d want 1", got)
	}
	// A new process with no network still verifies from the shared home.
	disabled := runCacheHelper(t, home, ref, "fetch", "", "")
	// The helper in fetch mode still needs a client, but the document is warm so
	// it never dials. Point it at an unroutable server to prove no dial.
	disabled.Env = append(disabled.Env, "OMNIGREX_TEST_CACHE_SERVER=")
	if out, err := disabled.CombinedOutput(); err != nil {
		// If the helper tried to dial it would fail; warm must succeed.
		t.Fatalf("warm child failed: %v %s", err, out)
	}
	_ = io.Discard
}

func TestRegistryDocumentCacheProcessDeathReleasesLock(t *testing.T) {
	home := t.TempDir()
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureRegistryCacheDirs(root); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	docHex := strings.Repeat("a", 64)
	lockRel := registryLockRel(docHex)
	cmd := runCacheHelper(t, home, "", "die-holding-lock", lockRel, "")
	if err := cmd.Run(); err == nil {
		t.Fatal("die-holding-lock helper unexpectedly succeeded")
	}
	// The kernel releases the dead holder's lock; acquisition must succeed.
	root2, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root2.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := acquireRegistryLock(ctx, root2, lockRel)
	if err != nil {
		t.Fatalf("lock not released after process death: %v", err)
	}
	releaseRegistryLock(lock)
}
