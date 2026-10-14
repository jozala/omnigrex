//go:build integration && microsandbox && cgo

package microsandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestNativeRegistryImagePullIntoFreshCache(t *testing.T) {
	if os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("requires qualified Linux/KVM host")
	}
	// The pinned SDK owns process-global configuration, so a different cache
	// must be qualified in a fresh process rather than rebinding that global.
	root := os.Getenv("OMNIGREX_NATIVE_FRESH_IMAGE_ROOT")
	if root == "" {
		var err error
		root, err = os.MkdirTemp("/tmp", "msb-pull-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNativeRegistryImagePullIntoFreshCache$", "-test.v", "-test.timeout=2m")
		command.Env = append(os.Environ(), "OMNIGREX_NATIVE_FRESH_IMAGE_ROOT="+root)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fresh-process registry qualification: %v\n%s", err, output)
		}
		return
	}
	options := Options{Executable: "/usr/local/bin/msb", Firmware: "/usr/local/lib/libkrunfw.so.5.6.1", Home: filepath.Join(root, "cache"), Namespace: "fresh-registry-pull", WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "states"), ToolRoot: filepath.Join(root, "tools"), CPUs: 1, MaximumDuration: time.Minute, RootDiskMiB: 1024}
	backend, err := New(options, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := backend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	image := testRequest().Image // Actual pinned public Linux/amd64 OCI index.
	platform := profile.Platform{OS: "linux", Arch: "amd64"}
	if err := backend.Available(ctx, image, platform); err == nil {
		t.Fatal("unprovisioned registry image reported available in fresh cache")
	}
	if err := (commandCLI{options}).Run(ctx, nil, io.Discard, "image", "pull", image); err != nil {
		t.Fatal(err)
	}
	if err := backend.Available(ctx, image, platform); err != nil {
		t.Fatalf("exact registry identity/platform linkage not preserved by native pull: %v", err)
	}
}
