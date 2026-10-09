//go:build integration && microsandbox && cgo && linux

package microsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"golang.org/x/sys/unix"
)

func TestMicrosandboxForceStopPreservesUnflushedTerminalEvidence(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	workspace := backend.location(request.Key, "workspace")
	state := backend.location(request.Key, "state")
	markers := map[string]string{
		filepath.Join(workspace, "required.txt"): "workspace-retained",
		filepath.Join(state, "session.json"):     `{"session":"retained"}`,
	}
	for path, content := range markers {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request.Workspace = execution.WorkspaceRef{Key: request.Key, Seed: strings.Repeat("a", 40)}
	request.State = execution.StateRef{
		ID: request.Key.AssignmentID, ParticipantID: request.Key.AssignmentID, SessionID: request.Key.AgentSessionID,
		Backend: backend.Binding(), Format: "opencode-data/v1", Image: request.Image, RuntimeProfileSHA256: strings.Repeat("b", 64),
	}
	request.Attachments = []execution.Attachment{{Resource: "workspace", Target: "/workspace"}, {Resource: "state", Target: "/state"}}
	request.Command = []string{"/bin/sh", "-ec", `test "$(cat /workspace/required.txt)" = workspace-retained
test "$(cat /state/session.json)" = '{"session":"retained"}'
printf ready
exec sleep 300`}
	engine, err := backend.New(request, backend.ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(); _ = backend.Close() })

	// Fixture disposal deliberately bypasses production removal authorization.
	// It uses only this observed identity and never depends on required writeback
	// or EnsureKeyAbsent, which must reject the unflushed, started environment.
	pidfd := -1
	digest := backend.specificationDigest(request)
	t.Cleanup(func() {
		if pidfd >= 0 {
			defer unix.Close(pidfd)
			if err := unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
				t.Errorf("resume exact fixture VMM during disposal: %v", err)
			}
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		actual, err := backend.inspect(cleanup, nameFor(request.Key))
		if errors.Is(err, execution.ErrNotFound) {
			return
		}
		if err != nil || !backend.owned(actual, request.Key, digest) {
			t.Errorf("resolve fixture ownership for disposal: %+v, %v", actual, err)
			if pidfd >= 0 {
				_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
			}
			return
		}
		if err := backend.platform.Stop(cleanup, actual); err != nil {
			t.Errorf("independent exact fixture termination: %v", err)
			if pidfd >= 0 {
				_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
			}
			return
		}
		if err := backend.platform.Remove(cleanup, actual); err != nil {
			t.Errorf("remove exact disposable fixture: %v", err)
		}
	})
	process, err := engine.Create(ctx, request, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Transport().Close() })
	if err := process.Start(ctx); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, 15*time.Second)
	interruptRead := context.AfterFunc(readyCtx, func() { _ = process.Transport().Close() })
	var ready [5]byte
	_, err = io.ReadFull(process.Transport(), ready[:])
	interruptRead()
	cancelReady()
	if err != nil || string(ready[:]) != "ready" {
		t.Fatalf("started primary process readiness = %q, %v", ready, err)
	}
	actual, err := backend.inspect(ctx, nameFor(request.Key))
	if err != nil || !backend.owned(actual, request.Key, digest) || actual.Status != "Running" {
		t.Fatalf("running fixture identity = %+v, %v", actual, err)
	}
	pidfd, err = forceStopFixturePIDFD(backend.options.Executable, actual)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGSTOP, nil, 0); err != nil {
		t.Fatalf("suspend exact fixture VMM: %v", err)
	}

	// A frozen VMM cannot answer guest writeback. Force termination must still
	// finish, without claiming that its unflushed workspace is terminal evidence.
	stopCtx, cancelStop := context.WithTimeout(ctx, 15*time.Second)
	start := time.Now()
	err = backend.EnsureKeyStopped(stopCtx, request.Key)
	elapsed := time.Since(start)
	cancelStop()
	if err != nil || elapsed > 20*time.Second {
		t.Fatalf("force stop with unavailable writeback = %v after %s", err, elapsed)
	}
	stopped, err := backend.resolveRef(ctx, resourceRef(actual))
	if err != nil || stopped.Status != "Stopped" && stopped.Status != "Crashed" {
		t.Fatalf("exact environment did not terminate: %+v, %v", stopped, err)
	}
	if events, err := unix.Poll([]unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}, 1000); err != nil || events == 0 {
		t.Fatalf("fixture VMM still exists after stop: poll events=%d, %v", events, err)
	}
	observation, err := backend.ObserveKeyExit(ctx, request.Key)
	if err != nil || !observation.Exited || observation.Running || observation.NeverStarted || observation.ExitCodeKnown || observation.OOMKilled || observation.Resource != resourceRef(actual) {
		t.Fatalf("forced termination fabricated process evidence: %+v, %v", observation, err)
	}
	sealed := false
	err = backend.ReadWorkspace(ctx, execution.CaptureRequest{
		Workspace: request.Workspace, OperationID: request.Key.AgentTurnID, Purpose: execution.TerminalCapture,
	}, func(io.Reader) error { sealed = true; return nil })
	if !errors.Is(err, execution.ErrUnknownEffect) || sealed {
		t.Fatalf("unflushed terminal capture = %v, sealing invoked=%t", err, sealed)
	}
	if err := backend.EnsureKeyAbsent(ctx, request.Key); !errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("production removal authorized an unflushed started environment: %v", err)
	}
	if retained, err := backend.resolveRef(ctx, resourceRef(actual)); err != nil || retained.ID != actual.ID {
		t.Fatalf("required disposable environment was discarded: %+v, %v", retained, err)
	}
	for path, want := range markers {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != want {
			t.Fatalf("required workspace/Session data was discarded: %s = %q, %v", path, content, err)
		}
	}
	t.Logf("PASS: exact suspended VMM stopped in %s; exit cause unknown, terminal capture unavailable, environment and required data retained", elapsed)
}

// Pin the process before signaling it and match the stable sandbox ID in the
// pinned runtime's argv. Other VMs, including concurrent fixtures, are
// never selected merely because their executable is msb; pidfd prevents PID reuse.
func forceStopFixturePIDFD(executable string, expected metadata) (int, error) {
	nativeID, local := strings.CutPrefix(expected.ID, "local:")
	if !local || nativeID == "" {
		return -1, fmt.Errorf("fixture has no local SDK identity: %s", expected.ID)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1, err
	}
	var matches []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		location := filepath.Join("/proc", entry.Name())
		if actual, err := os.Readlink(filepath.Join(location, "exe")); err != nil || actual != executable {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			continue
		}
		content, err := os.ReadFile(filepath.Join(location, "cmdline"))
		arguments := strings.Split(strings.TrimSuffix(string(content), "\x00"), "\x00")
		var name, id string
		if len(arguments) >= 2 && arguments[1] == "machine" {
			for index := 2; index+1 < len(arguments); index++ {
				switch arguments[index] {
				case "--name":
					name = arguments[index+1]
				case "--sandbox-id":
					id = arguments[index+1]
				}
			}
		}
		if err == nil && name == expected.Name && id == nativeID {
			matches = append(matches, fd)
		} else {
			_ = unix.Close(fd)
		}
	}
	if len(matches) != 1 {
		for _, fd := range matches {
			_ = unix.Close(fd)
		}
		return -1, fmt.Errorf("expected one exact fixture VMM for %s/%s, found %d", expected.Name, expected.ID, len(matches))
	}
	return matches[0], nil
}
