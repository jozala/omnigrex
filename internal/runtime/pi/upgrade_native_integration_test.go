//go:build integration && microsandbox && cgo

package pi_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func upgradeImages(t *testing.T) (string, string, profile.Platform, config.Config) {
	t.Helper()
	settings := config.Config{ExecutionBackend: "microsandbox", MicrosandboxExecutable: "/usr/local/bin/msb", MicrosandboxFirmware: "/usr/local/lib/libkrunfw.so.5.6.1", MicrosandboxHome: "/tmp/msb-upgrade", MicrosandboxNamespace: "upgrade-import", WorkspaceRoot: t.TempDir(), RuntimeStateRoot: t.TempDir(), MiseRoot: t.TempDir(), MicrosandboxCPUs: 1, MicrosandboxRootDiskMiB: 4096, AgentTurnExecutionTurnTimeout: time.Minute}
	b, err := microsandbox.New(composition.MicrosandboxOptions(settings), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if err := b.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	load := func(name string) string {
		image := os.Getenv("OMNIGREX_PI_UPGRADE_" + name + "_IMAGE")
		archive := "/tmp/pi-upgrade-" + name + ".tar"
		images, err := sdk.Image.Load(t.Context(), archive, "registry.example/omnigrex/pi-upgrade:fixture")
		if err != nil || len(images) == 0 {
			t.Fatalf("import upgrade image: %v", err)
		}
		if image == "" {
			image = "registry.example/omnigrex/pi-upgrade@" + images[0].ManifestDigest()
		}
		if _, err := sdk.Image.Load(t.Context(), archive, image); err != nil {
			t.Fatal(err)
		}
		return image
	}
	return load("SOURCE"), load("TARGET"), profile.Platform{OS: "linux", Arch: "amd64"}, settings
}

func upgradeNativeFactory(t *testing.T, settings config.Config, root, participant string) execution.Factory {
	t.Helper()
	settings.MicrosandboxNamespace = "upgrade-copy-" + uuid.NewString()
	settings.RuntimeStateRoot = root
	settings.WorkspaceRoot = t.TempDir()
	settings.MiseRoot = t.TempDir()
	for _, directory := range []string{filepath.Join(settings.WorkspaceRoot, "assignment-"+participant, "workspace"), filepath.Join(settings.MiseRoot, "assignment-"+participant, "mise"), filepath.Join(settings.MiseRoot, "assignment-"+participant, "tool-data")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	b, err := microsandbox.New(composition.MicrosandboxOptions(settings), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}
