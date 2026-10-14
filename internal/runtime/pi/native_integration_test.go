//go:build integration && microsandbox && cgo

package pi_test

import (
	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func nativeDiagnosticFixture(t *testing.T) (string, execution.Platform, config.Config) {
	t.Helper()
	settings := config.Config{ExecutionBackend: "microsandbox", MicrosandboxExecutable: "/usr/local/bin/msb", MicrosandboxFirmware: "/usr/local/lib/libkrunfw.so.5.6.1", MicrosandboxHome: "/tmp/msb-pi", MicrosandboxNamespace: "pi-diagnostic", WorkspaceRoot: "/tmp/pi-diagnostic-workspaces", RuntimeStateRoot: "/tmp/pi-diagnostic-states", MiseRoot: "/tmp/pi-diagnostic-tools", MicrosandboxCPUs: 1, MicrosandboxRootDiskMiB: 4096, AgentTurnExecutionTurnTimeout: time.Minute}
	backend, err := microsandbox.New(microsandbox.Options{Executable: settings.MicrosandboxExecutable, Firmware: settings.MicrosandboxFirmware, Home: settings.MicrosandboxHome, Namespace: settings.MicrosandboxNamespace, WorkspaceRoot: settings.WorkspaceRoot, StateRoot: settings.RuntimeStateRoot, ToolRoot: settings.MiseRoot, CPUs: 1, RootDiskMiB: 4096, MaximumDuration: 5 * time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	images, err := sdk.Image.Load(t.Context(), "/tmp/pi-image.tar", "registry.example/omnigrex/pi-diagnostic:fixture")
	if err != nil || len(images) == 0 {
		t.Fatalf("import Pi fixture: %v", err)
	}
	image := "registry.example/omnigrex/pi-diagnostic@" + images[0].ManifestDigest()
	if _, err := sdk.Image.Load(t.Context(), "/tmp/pi-image.tar", image); err != nil {
		t.Fatal(err)
	}
	return image, execution.Platform{OS: "linux", Architecture: "amd64"}, settings
}

func nativeSessionFactory(t *testing.T, settings config.Config) (execution.Factory, func(string)) {
	t.Helper()
	settings.MicrosandboxNamespace = "pi-sessions-" + uuid.NewString()
	settings.WorkspaceRoot = t.TempDir()
	settings.RuntimeStateRoot = t.TempDir()
	settings.MiseRoot = t.TempDir()
	backend, err := microsandbox.New(composition.MicrosandboxOptions(settings), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, func(participant string) {
		for _, entry := range []struct{ root, name string }{{settings.RuntimeStateRoot, "runtime-state"}, {settings.WorkspaceRoot, "workspace"}} {
			path := filepath.Join(entry.root, "assignment-"+participant, entry.name)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(path, 10001, 10001); err != nil {
				t.Fatal(err)
			}
		}
	}
}
