//go:build integration && microsandbox && cgo

package microsandbox_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func TestSDKProtocolAndCapture(t *testing.T) {
	if os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("requires qualified Linux/KVM host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Setenv("MSB_HOME", "/tmp/msb-sdk-probe")
	t.Setenv("MSB_PATH", "/usr/local/bin/msb")
	t.Setenv("MSB_LIBKRUNFW_PATH", "/usr/local/lib/libkrunfw.so.5.6.1")
	resolved, err := sdk.ResolveRuntime(sdk.RuntimeConfig{Home: "/tmp/msb-sdk-probe", MSBPath: "/usr/local/bin/msb", LibkrunfwPath: "/usr/local/lib/libkrunfw.so.5.6.1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("resolved runtime origin=%s", resolved.Origin)
	info, err := sdk.DefaultBackendInfo()
	if err != nil || info.Kind != sdk.BackendLocal {
		t.Fatalf("local backend: %+v, %v", info, err)
	}
	sandbox, err := sdk.CreateSandbox(ctx, "omnigrex-sdk-probe", sdk.WithImage(alpineImage), sdk.WithMemory(512), sdk.WithCPUs(1), sdk.WithRootDisk(sdk.RootDisk.Flat(sdk.RootDiskFlatOptions{SizeMiB: 4096})), sdk.WithUser("10001:10001"), sdk.WithWorkdir("/tmp"), sdk.WithEntrypoint("/bin/sh"), sdk.WithCmd(), sdk.WithMounts(map[string]sdk.MountConfig{"/run/omnigrex": sdk.Mount.Tmpfs(sdk.TmpfsOptions{SizeMiB: 16})}), sdk.WithMaxDuration(2*time.Minute), sdk.WithNetwork(sdk.NetworkPolicy.FromProfiles(sdk.NetworkProfilePublic)), sdk.WithDetached(), sdk.WithQuietLogs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		handle, err := sdk.GetSandbox(cleanup, "omnigrex-sdk-probe")
		if err == nil {
			if err := handle.Destroy(cleanup, sdk.WithDestroyForce()); err != nil {
				t.Error(err)
			}
		}
	})
	defer sandbox.Detach(context.Background())
	stream, err := sandbox.ExecStream(ctx, "/bin/sh", []string{"-c", `printf 'ready\n'; printf 'separate\n' >&2; while read -r line; do printf '%s\n' "$line"; done`}, sdk.WithExecStdinPipe())
	if err != nil {
		t.Fatal(err)
	}
	stdin := stream.TakeStdin()
	if stdin == nil {
		t.Fatal("missing stdin")
	}
	var out, stderr string
	for out != "ready\n" || stderr != "separate\n" {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case sdk.ExecEventStdout:
			out += string(event.Data)
		case sdk.ExecEventStderr:
			stderr += string(event.Data)
		}
	}
	if err := sandbox.PauseWithGuestFlush(ctx, sdk.GuestFlushRequired); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.WriteCtx(ctx, []byte("echo\n")); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == sdk.ExecEventStdout {
			if string(event.Data) != "echo\n" {
				t.Fatalf("roundtrip=%q", event.Data)
			}
			break
		}
	}
	// Cancellation must remain bounded without concurrent Recv calls. Check the
	// native call's cancellation result before closing the handle.
	readCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = stream.Recv(readCtx)
	stop()
	if err == nil {
		t.Fatal("idle Recv did not cancel")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "closed\n"); err == nil {
		t.Log("SDK input remains independent of detached event stream")
	}
	handle, err := sdk.GetSandbox(ctx, sandbox.Name())
	if err != nil || handle.Status() != sdk.SandboxStatusRunning {
		t.Fatalf("detach stopped environment: %v, %v", handle, err)
	}
	if err := handle.Kill(ctx, sdk.WithKillTimeout(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WaitUntilStopped(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS: SDK typed bidirectional non-TTY streams, separate stderr, required flush pause/resume, bounded Recv cancellation, detach, and environment-wide stop")
}
