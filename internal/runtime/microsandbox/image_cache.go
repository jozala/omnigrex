//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	mrand "math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

const (
	registryDocCacheBase     = "omnigrex-registry-documents/v1"
	registryDocSubdir        = "omnigrex-registry-documents/v1/documents"
	registryLockSubdir       = "omnigrex-registry-documents/v1/locks"
	registryCooldownSubdir   = "omnigrex-registry-documents/v1/cooldown"
	registryRetryStateLimit  = 4096
	registryBaseBackoff      = 30 * time.Second
	registryMaxBackoff       = 5 * time.Minute
	registryLockPollInterval = 10 * time.Millisecond
)

// registryDocumentCache persists immutable digest-addressed registry manifests
// and indexes under the configured Microsandbox home. A cache hit proves only
// the document's content identity; SDK metadata, configuration, platform, and
// index-linkage checks remain authoritative in verifyImageContent.
type registryDocumentCache struct {
	home   string
	client *http.Client
	now    func() time.Time
	jitter func() float64
}

func newRegistryDocumentCache(home string, client *http.Client) *registryDocumentCache {
	if client == nil {
		client = &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &registryDocumentCache{
		home:   home,
		client: client,
		now:    time.Now,
		jitter: func() float64 { return 0.8 + mrand.Float64()*0.4 },
	}
}

type registryRetryState struct {
	Version       int   `json:"version"`
	Failures      int   `json:"failures"`
	NextRetryUnix int64 `json:"next_retry_unix"`
}

func registryDocHex(digestStr string) (string, bool) {
	value := digest.Digest(digestStr)
	if value.Validate() != nil || value.Algorithm() != digest.SHA256 {
		return "", false
	}
	hexPart := strings.TrimPrefix(string(value), "sha256:")
	if len(hexPart) != 64 {
		return "", false
	}
	if decoded, err := hex.DecodeString(hexPart); err != nil || len(decoded) != 32 {
		return "", false
	}
	if strings.ToLower(hexPart) != hexPart {
		return "", false
	}
	return hexPart, true
}

func registryHostForRef(ref string) (string, error) {
	named, err := reference.ParseNamed(ref)
	if err != nil {
		return "", err
	}
	host := reference.Domain(named)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return strings.ToLower(host), nil
}

func registryDocRel(docHex string) string {
	return registryDocSubdir + "/" + docHex + ".json"
}

func registryLockRel(docHex string) string {
	return registryLockSubdir + "/" + docHex + ".lock"
}

func registryRefCooldownRel(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return registryCooldownSubdir + "/ref-" + hex.EncodeToString(sum[:]) + ".json"
}

func registryOriginCooldownRel(host string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host)))
	return registryCooldownSubdir + "/origin-" + hex.EncodeToString(sum[:]) + ".json"
}

func ensureRegistryCacheDirs(root *os.Root) error {
	for _, dir := range []string{registryDocCacheBase, registryDocSubdir, registryLockSubdir, registryCooldownSubdir} {
		if err := root.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}

func readVerifiedRegistryDocument(root *os.Root, docHex, expectedDigest string) ([]byte, error) {
	rel := registryDocRel(docHex)
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > imageMetadataLimit {
		return nil, errImageCorrupt
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errImageCorrupt
	}
	defer file.Close()
	if stat, err := file.Stat(); err != nil || !stat.Mode().IsRegular() || stat.Size() > imageMetadataLimit {
		return nil, errImageCorrupt
	}
	content, err := readImageDocument(file, imageMetadataLimit)
	if err != nil {
		return nil, err
	}
	if !matchesImageDigest(expectedDigest, content) {
		return nil, errImageCorrupt
	}
	return content, nil
}

func readRegistryRetryState(root *os.Root, rel string) (registryRetryState, bool) {
	var state registryRetryState
	info, err := root.Lstat(rel)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > registryRetryStateLimit {
		return state, false
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return state, false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, registryRetryStateLimit+1))
	if err != nil || int64(len(content)) > registryRetryStateLimit {
		return state, false
	}
	if json.Unmarshal(content, &state) != nil {
		return state, false
	}
	if state.Version != 1 || state.Failures < 1 || state.Failures > 100 || state.NextRetryUnix <= 0 {
		return state, false
	}
	return state, true
}

func registryTempName() string {
	var random [8]byte
	_, _ = rand.Read(random[:])
	return ".tmp-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(random[:])
}

func publishRegistryBytes(root *os.Root, dirRel, targetRel string, content []byte) error {
	tmpRel := dirRel + "/" + registryTempName()
	file, err := root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = root.Remove(tmpRel)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	failed = false
	if err := root.Rename(tmpRel, targetRel); err != nil {
		_ = root.Remove(tmpRel)
		return err
	}
	if dir, err := root.Open(dirRel); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func publishRegistryDocument(root *os.Root, docHex string, content []byte) error {
	return publishRegistryBytes(root, registryDocSubdir, registryDocRel(docHex), content)
}

func writeRegistryRetryState(root *os.Root, rel string, state registryRetryState) error {
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return publishRegistryBytes(root, registryCooldownSubdir, rel, content)
}

func acquireRegistryLock(ctx context.Context, root *os.Root, lockRel string) (*os.File, error) {
	// Remove only demonstrably unsafe entries before opening. A regular file
	// is never unlinked here: another process may still hold flock on its
	// inode, and recreating the path would create an independent lock.
	if info, err := root.Lstat(lockRel); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			_ = root.Remove(lockRel)
		} else if !info.Mode().IsRegular() {
			_ = root.RemoveAll(lockRel)
		}
	}
	file, err := root.OpenFile(lockRel, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		// An open failure on a regular file must not unlink the stable inode.
		// Only a symlink race (ELOOP with a confirmed symlink) may be
		// removed and retried; all other failures return a bounded error.
		if errors.Is(err, unix.ELOOP) {
			if info, lerr := root.Lstat(lockRel); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
				_ = root.Remove(lockRel)
				file, err = root.OpenFile(lockRel, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
			}
		}
		if err != nil {
			return nil, errors.Join(ctx.Err(), errImageUnavailable)
		}
	}
	for {
		if ctx.Err() != nil {
			_ = file.Close()
			return nil, errors.Join(ctx.Err(), errImageUnavailable)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return file, nil
		} else if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = file.Close()
			return nil, errors.Join(ctx.Err(), errImageUnavailable)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, errors.Join(ctx.Err(), errImageUnavailable)
		case <-time.After(registryLockPollInterval):
		}
	}
}

func releaseRegistryLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}

func (cache *registryDocumentCache) computeNextRetry(failures int, retryAfter time.Time) time.Time {
	now := cache.now()
	if failures < 1 {
		failures = 1
	}
	if failures > 10 {
		failures = 10
	}
	seconds := int64(registryBaseBackoff/time.Second) << (failures - 1)
	if capped := int64(registryMaxBackoff / time.Second); seconds > capped {
		seconds = capped
	}
	delay := time.Duration(float64(seconds) * cache.jitter() * float64(time.Second))
	if delay > registryMaxBackoff {
		delay = registryMaxBackoff
	}
	if delay <= 0 {
		delay = registryBaseBackoff
	}
	if !retryAfter.IsZero() && retryAfter.After(now) {
		if serverDelay := retryAfter.Sub(now); serverDelay > delay {
			delay = serverDelay
		}
	}
	return now.Add(delay)
}

func (cache *registryDocumentCache) inCooldown(root *os.Root, ref, host string) bool {
	now := cache.now()
	if state, ok := readRegistryRetryState(root, registryRefCooldownRel(ref)); ok {
		if now.Before(time.Unix(state.NextRetryUnix, 0)) {
			return true
		}
	}
	if host != "" {
		if state, ok := readRegistryRetryState(root, registryOriginCooldownRel(host)); ok {
			if now.Before(time.Unix(state.NextRetryUnix, 0)) {
				return true
			}
		}
	}
	return false
}

func (cache *registryDocumentCache) recordRefFailure(root *os.Root, ref string, retryAfter time.Time) {
	now := cache.now()
	rel := registryRefCooldownRel(ref)
	previous, ok := readRegistryRetryState(root, rel)
	failures := previous.Failures + 1
	if failures < 1 {
		failures = 1
	}
	next := cache.computeNextRetry(failures, retryAfter)
	// Overlapping responses must not shorten an existing active deadline.
	if ok {
		if prevDeadline := time.Unix(previous.NextRetryUnix, 0); prevDeadline.After(now) && prevDeadline.After(next) {
			next = prevDeadline
		}
	}
	_ = writeRegistryRetryState(root, rel, registryRetryState{Version: 1, Failures: failures, NextRetryUnix: next.Unix()})
}

func (cache *registryDocumentCache) recordOriginFailure(ctx context.Context, root *os.Root, host string, retryAfter time.Time) {
	sum := sha256.Sum256([]byte(strings.ToLower(host)))
	lockRel := registryLockSubdir + "/origin-" + hex.EncodeToString(sum[:]) + ".lock"
	lock, err := acquireRegistryLock(ctx, root, lockRel)
	if err != nil {
		return
	}
	defer releaseRegistryLock(lock)
	rel := registryOriginCooldownRel(host)
	now := cache.now()
	previous, ok := readRegistryRetryState(root, rel)
	failures := previous.Failures + 1
	if failures < 1 {
		failures = 1
	}
	next := cache.computeNextRetry(failures, retryAfter)
	// Overlapping 429 responses must not shorten an existing server-requested
	// origin deadline: retain the maximum of the active and new deadlines.
	if ok {
		if prevDeadline := time.Unix(previous.NextRetryUnix, 0); prevDeadline.After(now) && prevDeadline.After(next) {
			next = prevDeadline
		}
	}
	_ = writeRegistryRetryState(root, rel, registryRetryState{Version: 1, Failures: failures, NextRetryUnix: next.Unix()})
}

func (cache *registryDocumentCache) recordFetchFailure(ctx context.Context, root *os.Root, ref, host string, fetchErr error) {
	var typed *registryFetchError
	if errors.As(fetchErr, &typed) && typed.status == http.StatusTooManyRequests {
		cache.recordOriginFailure(ctx, root, host, typed.retryAfter)
		return
	}
	if errors.As(fetchErr, &typed) && !typed.retryAfter.IsZero() {
		cache.recordRefFailure(root, ref, typed.retryAfter)
		return
	}
	cache.recordRefFailure(root, ref, time.Time{})
}

// fetch satisfies the verifyImageContent document-fetch boundary with a
// persistent content-addressed cache. It performs at most one underlying
// exact-digest fetch per eligible lookup and never returns invalid bytes.
func (cache *registryDocumentCache) fetch(ctx context.Context, ref string) ([]byte, error) {
	if !profile.IsExactRegistryImage(ref) {
		return nil, execution.ErrInvalidSpec
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, errImageUnavailable)
	}
	named, err := reference.ParseNamed(ref)
	if err != nil || named.String() != ref {
		return nil, execution.ErrInvalidSpec
	}
	canonical, ok := named.(reference.Canonical)
	if !ok {
		return nil, execution.ErrInvalidSpec
	}
	digestStr := canonical.Digest().String()
	docHex, ok := registryDocHex(digestStr)
	if !ok {
		return nil, execution.ErrInvalidSpec
	}
	host, err := registryHostForRef(ref)
	if err != nil {
		return nil, execution.ErrInvalidSpec
	}
	root, err := os.OpenRoot(cache.home)
	if err != nil {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	defer root.Close()
	if err := ensureRegistryCacheDirs(root); err != nil {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	if content, err := readVerifiedRegistryDocument(root, docHex, digestStr); err == nil {
		return content, nil
	} else if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	if cache.inCooldown(root, ref, host) {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	lock, err := acquireRegistryLock(ctx, root, registryLockRel(docHex))
	if err != nil {
		return nil, err
	}
	defer releaseRegistryLock(lock)
	if content, err := readVerifiedRegistryDocument(root, docHex, digestStr); err == nil {
		return content, nil
	} else if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	// A waiter may have blocked while the holder recorded a shared retry
	// cooldown for this document. Re-check before fetching so concurrent
	// failures do not each issue their own immediate registry request.
	if cache.inCooldown(root, ref, host) {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	// A directory at the document target can never be replaced by atomic
	// file publication. Reject before fetching so a persistent local fault
	// does not download on every lookup. No retry state is needed: the
	// pre-fetch check itself bounds every lookup to zero downloads.
	if info, err := root.Lstat(registryDocRel(docHex)); err == nil && info.IsDir() {
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	content, fetchErr := fetchImageIndexWithClient(ctx, ref, cache.client)
	if fetchErr != nil {
		cache.recordFetchFailure(ctx, root, ref, host, fetchErr)
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	if int64(len(content)) > imageMetadataLimit || !matchesImageDigest(digestStr, content) {
		cache.recordRefFailure(root, ref, time.Time{})
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	// A persistent write failure must not become a silent repeated-fetch path.
	// Record bounded retry suppression so subsequent lookups do not all
	// re-download before failing to publish.
	if err := publishRegistryDocument(root, docHex, content); err != nil {
		cache.recordRefFailure(root, ref, time.Time{})
		return nil, errors.Join(ctx.Err(), errImageUnavailable)
	}
	_ = root.Remove(registryRefCooldownRel(ref))
	return content, nil
}
