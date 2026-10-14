package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	imageTypes "github.com/moby/moby/api/types/image"
	mobyclient "github.com/moby/moby/client"
)

func TestExactImageAvailabilityDockerAPIVersions(t *testing.T) {
	for _, tc := range []struct {
		name, apiVersion, override, os, arch string
		platformOption                       bool
		status                               int
		wantErr                              bool
	}{
		{name: "minimum supported API", apiVersion: "1.45", os: "linux", arch: "arm64"},
		{name: "CI API", apiVersion: "1.48", os: "linux", arch: "arm64"},
		{name: "platform selection API", apiVersion: "1.49", os: "linux", arch: "arm64", platformOption: true},
		{name: "new API", apiVersion: "1.55", os: "linux", arch: "arm64", platformOption: true},
		{name: "explicit older API", apiVersion: "1.55", override: "1.48", os: "linux", arch: "arm64"},
		{name: "wrong OS", apiVersion: "1.48", os: "windows", arch: "arm64", wantErr: true},
		{name: "wrong architecture", apiVersion: "1.48", os: "linux", arch: "amd64", wantErr: true},
		{name: "new API wrong platform", apiVersion: "1.55", os: "linux", arch: "amd64", platformOption: true, wantErr: true},
		{name: "missing image", apiVersion: "1.48", status: http.StatusNotFound, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
			inspected := false
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("API-Version", tc.apiVersion)
				if strings.HasSuffix(r.URL.Path, "/_ping") {
					_, _ = w.Write([]byte("OK"))
					return
				}
				effective := tc.apiVersion
				if tc.override != "" {
					effective = tc.override
				}
				if r.Method != http.MethodGet || r.URL.Path != "/v"+effective+"/images/"+image+"/json" {
					t.Errorf("unexpected image inspection: %s %s", r.Method, r.URL)
				}
				inspected = true
				selection := r.URL.Query().Get("platform")
				if tc.platformOption {
					var platform struct{ OS, Architecture string }
					if err := json.Unmarshal([]byte(selection), &platform); err != nil || platform.OS != "linux" || platform.Architecture != "arm64" {
						t.Errorf("platform selection = %q: %v", selection, err)
					}
				} else if selection != "" {
					t.Errorf("unsupported platform selection: %s", selection)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"message":"image unavailable"}`))
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]string{"Id": "sha256:fixture", "Os": tc.os, "Architecture": tc.arch}); err != nil {
					t.Error(err)
				}
			}))
			defer daemon.Close()
			t.Setenv("DOCKER_HOST", daemon.URL)
			t.Setenv("DOCKER_API_VERSION", tc.override)
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			availability, err := NewExactImageAvailability()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = availability.Close() }()
			err = availability.Available(t.Context(), image, runtimeprofile.Platform{OS: "linux", Arch: "arm64"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Available() = %v, want error %t", err, tc.wantErr)
			}
			if !inspected {
				t.Fatal("exact image was not inspected")
			}
		})
	}
}

type fakeImageAvailabilityAPI struct {
	image       string
	optionCount int
	err         error
}

func (api *fakeImageAvailabilityAPI) Ping(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error) {
	return mobyclient.PingResult{APIVersion: "1.49"}, nil
}

func (api *fakeImageAvailabilityAPI) ClientVersion() string { return "1.49" }

func (api *fakeImageAvailabilityAPI) ImageInspect(_ context.Context, image string, options ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error) {
	api.image = image
	api.optionCount = len(options)
	return mobyclient.ImageInspectResult{InspectResponse: imageTypes.InspectResponse{Os: "linux", Architecture: "arm64"}}, api.err
}

func TestExactImageAvailabilityInspectsRegistryDigestForPlatform(t *testing.T) {
	api := &fakeImageAvailabilityAPI{}
	closed := false
	availability := newExactImageAvailability(api, func() error { closed = true; return nil })
	image := "registry.example/omnigrex/pi@sha256:" + strings.Repeat("a", 64)
	if err := availability.Available(context.Background(), image, runtimeprofile.Platform{OS: "linux", Arch: "arm64"}); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	if api.image != image || api.optionCount != 1 {
		t.Fatalf("ImageInspect() = (%q, %d options), want exact image and platform option", api.image, api.optionCount)
	}
	if err := availability.Close(); err != nil || !closed {
		t.Fatalf("Close() = %v, closed %t", err, closed)
	}
}

func TestExactImageAvailabilityRejectsMutableTagsAndLocalIDs(t *testing.T) {
	for _, image := range []string{
		"registry.example/omnigrex/pi:latest",
		"sha256:" + strings.Repeat("a", 64),
	} {
		api := &fakeImageAvailabilityAPI{}
		err := newExactImageAvailability(api, func() error { return nil }).Available(context.Background(), image, runtimeprofile.Platform{OS: "linux", Arch: "amd64"})
		if !errors.Is(err, ErrInvalidExactImageAvailability) {
			t.Errorf("Available(%q) error = %v, want ErrInvalidExactImageAvailability", image, err)
		}
		if api.image != "" {
			t.Errorf("ImageInspect() called for non-exact image %q", image)
		}
	}
}
