//go:build integration && microsandbox && cgo && linux

package microsandbox

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestNativeSetupExecutionCompletion(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	request.Command = []string{"/bin/sleep", "300"}
	process := launchFixture(t, ctx, backend, request)
	actual, err := backend.inspect(ctx, nameFor(request.Key))
	if err != nil {
		t.Fatal(err)
	}
	// These commands may finish before the caller receives the exec handle.
	// Successful completion, not a subsequent stdin close, proves their effect.
	for i := range 256 {
		if err := backend.platform.Execute(ctx, actual, []string{"/bin/true"}, request.User, nil); err != nil {
			t.Fatalf("fast setup execution %d: %v", i, err)
		}
	}
	if err := backend.platform.Execute(ctx, actual, []string{"/bin/sh", "-ec", "test -z \"$(cat)\""}, request.User, nil); err != nil {
		t.Fatalf("setup without input must receive EOF: %v", err)
	}
	if err := backend.platform.Execute(ctx, actual, []string{"/bin/sh", "-ec", "read -r value; test \"$value\" = setup-input; test -z \"$(cat)\""}, request.User, strings.NewReader("setup-input\n")); err != nil {
		t.Fatalf("setup input and EOF: %v", err)
	}
	if err := backend.platform.Execute(ctx, actual, []string{"/bin/sh", "-ec", "exit 7"}, request.User, nil); err == nil {
		t.Fatal("nonzero setup exit accepted")
	}
	if err := process.Stop(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}
