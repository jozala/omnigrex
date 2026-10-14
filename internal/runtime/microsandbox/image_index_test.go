//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type imageTestTransport func(*http.Request) (*http.Response, error)

func (transport imageTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func imageTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestImmutableIndexFetchUsesExactDigestAndAnonymousPullOnly(t *testing.T) {
	ref, _, _, index := imageProofFixture(t)
	calls := 0
	client := &http.Client{Transport: imageTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Scheme != "https" {
			t.Errorf("unexpected registry request: %s %s", request.Method, request.URL)
		}
		if calls == 2 {
			if request.URL.Host != "auth.example" || request.URL.Query().Get("scope") != "repository:fixture:pull" || request.URL.Query().Get("service") != "registry.example" || request.Header.Get("Authorization") != "" {
				t.Errorf("unsafe token request: %s", request.URL)
			}
			return imageTestResponse(http.StatusOK, `{"token":"anonymous-pull-token"}`), nil
		}
		if request.URL.String() != "https://registry.example/v2/fixture/manifests/"+strings.Split(ref, "@")[1] {
			t.Errorf("not the pinned digest endpoint: %s", request.URL)
		}
		if !strings.Contains(request.Header.Get("Accept"), "manifest.list.v2") {
			t.Error("Docker manifest lists not requested")
		}
		if calls == 1 {
			if request.Header.Get("Authorization") != "" {
				t.Error("initial registry request carried credentials")
			}
			response := imageTestResponse(http.StatusUnauthorized, "provider-private-key")
			response.Header.Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",service="registry.example",scope="repository:other:push"`)
			return response, nil
		}
		if request.Header.Get("Authorization") != "Bearer anonymous-pull-token" {
			t.Error("missing anonymous bearer authorization")
		}
		return imageTestResponse(http.StatusOK, string(index)), nil
	})}
	content, err := fetchImageIndexWithClient(context.Background(), ref, client)
	if err != nil || string(content) != string(index) || calls != 3 {
		t.Fatalf("fetch=%q, %v; calls=%d", content, err, calls)
	}
}

func TestImmutableIndexFetchRejectsUnboundedOrUnsafeResponses(t *testing.T) {
	ref, _, _, _ := imageProofFixture(t)
	for _, test := range []struct {
		name     string
		response *http.Response
		want     error
	}{
		{"oversized index", imageTestResponse(http.StatusOK, strings.Repeat("x", imageMetadataLimit+1)), errImageCorrupt},
		{"provider response", imageTestResponse(http.StatusForbidden, "provider-private-key"), errImageUnavailable},
		{"no mutable fallback", imageTestResponse(http.StatusNotFound, "guest-config"), errImageUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: imageTestTransport(func(*http.Request) (*http.Response, error) { return test.response, nil })}
			_, err := fetchImageIndexWithClient(context.Background(), ref, client)
			if !errors.Is(err, test.want) || strings.Contains(fmt.Sprintf("%+v", err), "private-key") {
				t.Fatalf("unsafe response accepted/retained: %v", err)
			}
		})
	}
	for _, challenge := range []string{
		`Basic realm="https://auth.example"`,
		`Bearer realm="http://auth.example"`,
		`Bearer realm="https://user:private-key@auth.example"`,
		`Bearer realm="https://auth.example",realm="https://other.example"`,
	} {
		client := &http.Client{Transport: imageTestTransport(func(*http.Request) (*http.Response, error) {
			t.Error("unsafe challenge dispatched")
			return nil, errors.New("private-key")
		})}
		if _, err := anonymousImageToken(context.Background(), client, challenge, "fixture"); !errors.Is(err, errImageUnavailable) {
			t.Fatalf("unsafe challenge accepted: %v", err)
		}
	}
}

func TestImmutableIndexFetchCancellationAndSourceErrorDoNotLeak(t *testing.T) {
	ref, _, _, _ := imageProofFixture(t)
	source := errors.New("provider-config-private-key")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	client := &http.Client{Transport: imageTestTransport(func(request *http.Request) (*http.Response, error) { <-request.Context().Done(); return nil, source })}
	start := time.Now()
	_, err := fetchImageIndexWithClient(ctx, ref, client)
	if !errors.Is(err, errImageUnavailable) || errors.Is(err, source) || strings.Contains(fmt.Sprintf("%+v", err), "private-key") || time.Since(start) > time.Second {
		t.Fatalf("unbounded/unsafe cancelled fetch: %v", err)
	}
	if _, err := fetchImageIndexWithClient(ctx, "registry.example/fixture:latest", client); !errors.Is(err, execution.ErrInvalidSpec) {
		t.Fatalf("mutable reference reached fetch: %v", err)
	}
}
