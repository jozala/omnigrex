//go:build integration && microsandbox && cgo

package store_test

import (
	"github.com/jozala/omnigrex/internal/runtime/microsandbox"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func workflowQualificationImage(t *testing.T) (string, profile.Platform) {
	t.Helper()
	if os.Getenv("OMNIGREX_PI_BACKEND") != "microsandbox" {
		return os.Getenv("OMNIGREX_PI_WORKFLOW_IMAGE"), profile.Platform{OS: "linux", Arch: os.Getenv("OMNIGREX_PI_WORKFLOW_ARCH")}
	}
	root := os.Getenv("OMNIGREX_PI_WORKFLOW_ROOT")
	backend, err := microsandbox.New(microsandbox.Options{Executable: "/usr/local/bin/msb", Firmware: "/usr/local/lib/libkrunfw.so.5.6.1", Home: "/tmp/msb-workflow", Namespace: "workflow-import", WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "state"), ToolRoot: filepath.Join(root, "tools"), CPUs: 1, RootDiskMiB: 4096, MaximumDuration: 10 * time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	if err := backend.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	images, err := sdk.Image.Load(t.Context(), "/tmp/pi-workflow-image.tar", "registry.example/omnigrex/pi-workflow:qualification")
	if err != nil || len(images) == 0 {
		t.Fatalf("import exact fixture: %v", err)
	}
	image := "registry.example/omnigrex/pi-workflow@" + images[0].ManifestDigest()
	if _, err := sdk.Image.Load(t.Context(), "/tmp/pi-workflow-image.tar", image); err != nil {
		t.Fatal(err)
	}
	return image, profile.Platform{OS: "linux", Arch: "amd64"}
}
