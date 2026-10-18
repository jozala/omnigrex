//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

var imageAuthParameters = regexp.MustCompile(`(?:^|[, ])(realm|service)="([^"]*)"`)

// registryFetchError carries a sanitized registry failure with its HTTP status
// and any valid Retry-After time for shared cooldown bookkeeping.
// Its Error string never includes authorization headers, tokens, raw provider
// bodies, or transport details.
type registryFetchError struct {
	status     int
	retryAfter time.Time
}

func (err *registryFetchError) Error() string { return errImageUnavailable.Error() }

func (err *registryFetchError) Unwrap() error { return errImageUnavailable }

// parseRegistryRetryAfter accepts delta-seconds or an HTTP date.
// A past date or malformed value is not a valid retry time.
func parseRegistryRetryAfter(header string, now time.Time) (time.Time, bool) {
	value := strings.TrimSpace(header)
	if value == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return time.Time{}, false
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	if when, err := http.ParseTime(value); err == nil {
		if when.After(now) {
			return when, true
		}
	}
	return time.Time{}, false
}

func registryUnavailableWithStatus(status int, response *http.Response, now time.Time) error {
	var retryAfter time.Time
	if response != nil {
		if when, ok := parseRegistryRetryAfter(response.Header.Get("Retry-After"), now); ok {
			retryAfter = when
		}
	}
	return &registryFetchError{status: status, retryAfter: retryAfter}
}

// The SDK drops parent indexes and reserializes registry manifests in its
// cache. Fetch only the exact digest document, over HTTPS, with anonymous pull
// authorization if necessary.
// No Docker config, registry password, or credential helper is consulted.
func fetchImageIndex(ctx context.Context, ref string) ([]byte, error) {
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return fetchImageIndexWithClient(ctx, ref, client)
}

func fetchImageIndexWithClient(ctx context.Context, ref string, client *http.Client) ([]byte, error) {
	if !profile.IsExactRegistryImage(ref) {
		return nil, execution.ErrInvalidSpec
	}
	named, _ := reference.ParseNamed(ref)
	host := reference.Domain(named)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	repository := reference.Path(named)
	endpoint := "https://" + host + "/v2/" + repository + "/manifests/" + named.(reference.Canonical).Digest().String()
	get := func(token string) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, errImageUnavailable
		}
		request.Header.Set("Accept", oci.MediaTypeImageIndex+", application/vnd.docker.distribution.manifest.list.v2+json, "+oci.MediaTypeImageManifest+", application/vnd.docker.distribution.manifest.v2+json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, errImageUnavailable
		}
		return response, nil
	}
	response, err := get("")
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		response.Body.Close()
		token, err := anonymousImageToken(ctx, client, challenge, repository)
		if err != nil {
			return nil, err
		}
		response, err = get(token)
		if err != nil {
			return nil, err
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, registryUnavailableWithStatus(response.StatusCode, response, time.Now())
	}
	return readImageDocument(response.Body, imageMetadataLimit)
}

func anonymousImageToken(ctx context.Context, client *http.Client, challenge, repository string) (string, error) {
	if len(challenge) > 4096 || !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", errImageUnavailable
	}
	parameters := map[string]string{}
	for _, match := range imageAuthParameters.FindAllStringSubmatch(challenge[len("Bearer "):], -1) {
		if _, duplicate := parameters[match[1]]; duplicate {
			return "", errImageUnavailable
		}
		parameters[match[1]] = match[2]
	}
	realm, err := url.Parse(parameters["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" || realm.User != nil || realm.Fragment != "" {
		return "", errImageUnavailable
	}
	query := realm.Query()
	query.Set("scope", "repository:"+repository+":pull")
	query.Set("service", parameters["service"])
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", errImageUnavailable
	}
	response, err := client.Do(request)
	if err != nil {
		return "", errImageUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", registryUnavailableWithStatus(response.StatusCode, response, time.Now())
	}
	content, err := readImageDocument(response.Body, 64<<10)
	if err != nil {
		return "", err
	}
	var result struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(content, &result) != nil {
		return "", errImageUnavailable
	}
	if result.Token == "" {
		result.Token = result.AccessToken
	}
	if result.Token == "" || strings.ContainsAny(result.Token, "\r\n") {
		return "", errImageUnavailable
	}
	return result.Token, nil
}
