//go:build integration && microsandbox && cgo

package pi_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func providerSmokeEnvironment(t *testing.T, root, endpoint string) liveEnvironment {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	workspace, states, tools := filepath.Join(root, "workspaces"), filepath.Join(root, "states"), filepath.Join(root, "tools")
	for _, directory := range []string{workspace, states, tools} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := microsandbox.New(microsandbox.Options{Executable: "/usr/local/bin/msb", Firmware: "/usr/local/lib/libkrunfw.so.5.6.1", Home: home, Namespace: "pi-live-" + uuid.NewString(), WorkspaceRoot: workspace, StateRoot: states, ToolRoot: tools, MCPEndpoint: endpoint, CPUs: 1, RootDiskMiB: 4096, MaximumDuration: 5 * time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	images, err := sdk.Image.Load(t.Context(), "/tmp/pi-live-image.tar", "registry.example/omnigrex/pi-live:fixture")
	if err != nil || len(images) == 0 {
		t.Fatal("cannot import live golden image")
	}
	image := "registry.example/omnigrex/pi-live@" + images[0].ManifestDigest()
	if _, err := sdk.Image.Load(t.Context(), "/tmp/pi-live-image.tar", image); err != nil {
		t.Fatal("cannot bind exact live golden image")
	}
	p, err := profile.NewPiV2(image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	return liveEnvironment{profile: p, factory: backend, image: image, binding: backend.Binding(), stateRoot: states, workspaceRoot: workspace, toolRoot: tools}
}
