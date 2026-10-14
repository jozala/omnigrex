//go:build integration && microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func TestNativeImmutableImageRejectsImportedDigestAliasAndDirectCreate(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	alias := "registry.example/omnigrex/alias@sha256:" + strings.Repeat("f", 64)
	images, err := sdk.Image.Load(ctx, "/tmp/golden-image.tar", alias)
	if err != nil || len(images) == 0 {
		t.Fatalf("load alias fixture: %v", err)
	}
	if images[0].ManifestDigest() == "sha256:"+strings.Repeat("f", 64) {
		t.Fatal("fixture unexpectedly has alias digest")
	}
	if _, _, err := backend.platform.Image(ctx, alias); err == nil {
		t.Fatal("digest-shaped imported alias passed image diagnostics")
	}
	request.Image = alias
	name := "omnigrex-rejected-image-alias"
	for _, diagnostic := range []bool{false, true} {
		if _, err := backend.platform.Create(ctx, creation{Request: request, Name: name, Diagnostic: diagnostic}); err == nil {
			t.Fatal("direct SDK create accepted imported digest alias")
		}
	}
	if _, err := backend.platform.Lookup(ctx, name); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("rejected image dispatched VM creation: %v", err)
	}
	// The same imported bytes are valid under their actual platform-manifest digest.
	exact := "registry.example/omnigrex/alias@" + images[0].ManifestDigest()
	if _, err := sdk.Image.Load(ctx, "/tmp/golden-image.tar", exact); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.platform.Image(ctx, exact); err != nil {
		t.Fatalf("exact imported platform manifest rejected: %v", err)
	}
	request.Image = exact
	request.Platform = execution.Platform{} // Existing diagnostic call shape.
	meta, err := backend.platform.Create(ctx, creation{Request: request, Name: name + "-exact", Diagnostic: true})
	if err != nil {
		t.Fatalf("direct diagnostic create rejected exact imported fixture: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := backend.platform.Stop(cleanup, meta); err != nil {
			t.Error(err)
		}
		if err := backend.platform.Remove(cleanup, meta); err != nil {
			t.Error(err)
		}
	})
}

func TestNativeImmutableIndexAndTransportDetach(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.platform.Image(ctx, request.Image); err != nil {
		t.Fatalf("pinned Alpine OCI index rejected: %v", err)
	}
	const dockerImage = "docker.io/library/docker@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6"
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", dockerImage); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.platform.Image(ctx, dockerImage); err != nil {
		t.Fatalf("pinned Docker OCI index rejected: %v", err)
	}
	meta, err := backend.platform.Create(ctx, creation{Request: request, Name: nameFor(request.Key), Labels: map[string]string{namespaceLabel: backend.options.Namespace}})
	if err != nil {
		t.Fatalf("direct create with validated index: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := backend.platform.Stop(cleanup, meta); err != nil {
			t.Error(err)
		}
		if err := backend.platform.Remove(cleanup, meta); err != nil {
			t.Error(err)
		}
	})
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-c", "cat >/tmp/input-proof"}, request.User, strings.NewReader("bounded-input")); err != nil {
		t.Fatalf("setup stdin release: %v", err)
	}
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-c", "test \"$(cat /tmp/input-proof)\" = bounded-input"}, request.User, nil); err != nil {
		t.Fatalf("setup streaming without stdin: %v", err)
	}
	setupCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	start := time.Now()
	err = backend.platform.Execute(setupCtx, meta, []string{"/bin/sh", "-c", "sleep 300"}, request.User, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 7*time.Second {
		t.Fatalf("cancelled setup did not return with bounded release: %v", err)
	}
	transport, err := backend.platform.Stream(ctx, meta, []string{"/bin/sh", "-c", "sleep 300"}, request.User)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("repeated transport close: %v", err)
	}
	observed, err := backend.platform.Lookup(ctx, meta.Name)
	if err != nil || observed.ID != meta.ID || observed.Status != "Running" {
		t.Fatalf("transport closure stopped environment: %+v, %v", observed, err)
	}
}
