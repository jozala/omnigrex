package composition

import (
	"context"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestConfiguredQualificationExplainsSelectionWithoutChangingInspectedImage(t *testing.T) {
	image := "registry.example/derived@sha256:" + strings.Repeat("a", 64)
	bundle, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := profile.NewPiV2(image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	// Invalid backend options fail before native dispatch; the failure still
	// identifies the configured binding, independently of the selected digest.
	err = CheckConfiguredRepositoryEnvironment(context.Background(), runtime, config.Config{ExecutionBackend: "microsandbox"})
	if err == nil {
		t.Fatal("expected unavailable backend")
	}
	for _, want := range []string{image, bundle.Image, bundle.SourceCommit, "selected production Pi bundle", "explicitly provision", "not the selection", "inventory=sha256:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	retainedErr := CheckRepositoryEnvironment(context.Background(), runtime, config.Config{ExecutionBackend: "microsandbox"})
	if retainedErr == nil || strings.Contains(retainedErr.Error(), "selected production") {
		t.Fatal(retainedErr)
	}
}
