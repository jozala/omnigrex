package profile_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/server"
)

type bindingSource struct {
	bindings []profile.Binding
	err      error
}

func (source bindingSource) ListProtectedRuntimeBindings(context.Context) ([]profile.Binding, error) {
	return source.bindings, source.err
}

type imageAvailability struct {
	missing string
	checked []string
}

func (images *imageAvailability) Available(_ context.Context, image string, platform profile.Platform) error {
	images.checked = append(images.checked, image+" "+platform.OS+"/"+platform.Arch)
	if image == images.missing {
		return errors.New("credential-sentinel registry response")
	}
	return nil
}

func TestAvailabilityCheckerChecksDistinctExactProtectedImages(t *testing.T) {
	first := availabilityProfile(t, "a", "arm64")
	second := availabilityProfile(t, "b", "amd64")
	catalog, err := profile.NewCatalog([]profile.Profile{first}, []profile.Profile{second})
	if err != nil {
		t.Fatal(err)
	}
	images := &imageAvailability{}
	checker, err := profile.NewAvailabilityChecker(bindingSource{bindings: []profile.Binding{
		first.Binding(), first.Binding(), second.Binding(),
	}}, catalog, images)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(images.checked) != 2 {
		t.Fatalf("checked images = %v, want two distinct digest/platform pairs", images.checked)
	}
}

func TestAvailabilityCheckerFailsReadinessWithoutDisclosingRegistryError(t *testing.T) {
	value := availabilityProfile(t, "c", "arm64")
	catalog, err := profile.NewCatalog([]profile.Profile{value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := &imageAvailability{missing: value.Contract().Image}
	checker, err := profile.NewAvailabilityChecker(bindingSource{bindings: []profile.Binding{value.Binding()}}, catalog, images)
	if err != nil {
		t.Fatal(err)
	}
	err = checker.Check(context.Background())
	if !errors.Is(err, profile.ErrProtectedImageUnavailable) {
		t.Fatalf("Check() error = %v, want ErrProtectedImageUnavailable", err)
	}
	if strings.Contains(err.Error(), "credential-sentinel") || strings.Contains(err.Error(), value.Contract().Image) {
		t.Fatalf("Check() disclosed verbose source diagnostic: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	server.Handler(checker, nil).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestAvailabilityCheckerRejectsUncataloguedProtectedBindingBeforeImageInspect(t *testing.T) {
	current := availabilityProfile(t, "d", "arm64")
	historical := availabilityProfile(t, "e", "arm64")
	catalog, err := profile.NewCatalog([]profile.Profile{current}, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := &imageAvailability{}
	unknown := historical.Binding()
	unknown.ContentSHA256 = strings.Repeat("f", 64)
	checker, err := profile.NewAvailabilityChecker(bindingSource{bindings: []profile.Binding{unknown}}, catalog, images)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(context.Background()); !errors.Is(err, profile.ErrProtectedBindingUnavailable) {
		t.Fatalf("Check() error = %v, want ErrProtectedBindingUnavailable", err)
	}
	if len(images.checked) != 0 {
		t.Fatalf("checked images after configuration conflict = %v", images.checked)
	}
}

func TestAvailabilityCheckerCanFailStartupOnBindingWithoutInspectingImages(t *testing.T) {
	current := availabilityProfile(t, "1", "arm64")
	unknown := current.Binding()
	unknown.ContentSHA256 = strings.Repeat("2", 64)
	catalog, err := profile.NewCatalog([]profile.Profile{current}, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := &imageAvailability{missing: current.Contract().Image}
	checker, err := profile.NewAvailabilityChecker(bindingSource{bindings: []profile.Binding{unknown}}, catalog, images)
	if err != nil {
		t.Fatal(err)
	}

	if err := checker.CheckBindings(context.Background()); !errors.Is(err, profile.ErrProtectedBindingUnavailable) {
		t.Fatalf("CheckBindings() error = %v, want ErrProtectedBindingUnavailable", err)
	}
	if len(images.checked) != 0 {
		t.Fatalf("CheckBindings() inspected images: %v", images.checked)
	}
}

func availabilityProfile(t *testing.T, digit, arch string) profile.Profile {
	t.Helper()
	value, err := profile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:"+strings.Repeat(digit, 64),
		profile.Platform{OS: "linux", Arch: arch},
	)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
