//go:build integration && microsandbox && cgo

package microsandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func nativeFixture(t *testing.T) (*Backend, execution.Request, context.Context) {
	t.Helper()
	if os.Getenv("OMNIGREX_MICROSANDBOX_INTEGRATION") != "1" {
		t.Skip("requires qualified Linux/KVM host")
	}
	root, err := os.MkdirTemp("/tmp", "msb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	options := Options{Executable: "/usr/local/bin/msb", Firmware: "/usr/local/lib/libkrunfw.so.5.6.1", Home: "/tmp/msb-step08", Namespace: "conformance", WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "states"), ToolRoot: filepath.Join(root, "tools"), CPUs: 2, MaximumDuration: 20 * time.Minute, RootDiskMiB: 20480}
	backend, err := New(options, conformance.NewLedger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	t.Cleanup(cancel)
	if err := backend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.Key.AgentTurnID = fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
	return backend, request, ctx
}

func launchFixture(t *testing.T, ctx context.Context, backend *Backend, request execution.Request) execution.Process {
	t.Helper()
	engine, err := backend.New(request, backend.ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := engine.EnsureAbsent(cleanup); err != nil {
			t.Error(err)
		}
	})
	process, err := engine.Create(ctx, request, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Transport().Close() })
	return process
}

func TestWorkspaceCaptureDuringAuthenticatedOutstandingCallAndRetainedState(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Header.Get("Authorization") != "Bearer fixture-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		held <- struct{}{}
		select {
		case <-release:
			_, _ = writer.Write([]byte("mcp-ok"))
		case <-incoming.Context().Done():
		}
	}))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	host := ""
	for _, address := range interfaces {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			host = ip.String()
			break
		}
	}
	if host == "" {
		t.Fatal("no reachable host IPv4 address")
	}
	endpoint := "http://" + net.JoinHostPort(host, strconvPort(listener.Addr())) + "/mcp"
	backend.options.MCPEndpoint = endpoint
	backend.platform = sdkPlatformForOptions(t, backend.options)
	workspace := backend.location(request.Key, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	request.Workspace = execution.WorkspaceRef{Key: request.Key, Seed: strings.Repeat("a", 40)}
	ref := execution.StateRef{ID: request.Key.AssignmentID, ParticipantID: request.Key.AssignmentID, SessionID: request.Key.AgentSessionID, Backend: backend.Binding(), Format: "opencode-data/v1", Image: request.Image, RuntimeProfileSHA256: strings.Repeat("b", 64)}
	source := &stateFixtureSource{ref: ref}
	states, err := dirstate.New(backend.options.StateRoot, backend.Binding(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backend.options.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := states.Ensure(ctx, ref); err != nil {
		t.Fatal(err)
	}
	request.State = ref
	request.Attachments = []execution.Attachment{{Resource: "workspace", Target: "/workspace"}, {Resource: "state", Target: "/state"}}
	request.Environment = []string{`OPENCODE_AUTH_CONTENT={"key":"provider-fixture-secret"}`}
	request.Command = []string{"/bin/sh", "-ec", `printf '{"session":"retained"}' > /state/session.json
printf executable > /workspace/executable; chmod 0755 /workspace/executable; ln -s executable /workspace/link
(while :; do head -c 1048576 /dev/zero | tr '\000' X > /workspace/.generation; mv /workspace/.generation /workspace/generation; sleep .01; done) &
printf '{"ready":true}\n'
wget -qO- --header='Authorization: Bearer fixture-token' ` + quote(endpoint) + `
printf '\n{"completed":true}\n'
sleep 300
`}
	process := launchFixture(t, ctx, backend, request)
	reader := bufio.NewReader(process.Transport())
	if line, err := reader.ReadString('\n'); err != nil || line != "{\"ready\":true}\n" {
		t.Fatalf("startup=%q,%v", line, err)
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("authenticated MCP request did not arrive")
	}
	for deadline := time.Now().Add(time.Second); ; {
		if _, err := os.Stat(filepath.Join(workspace, "generation")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workspace writer did not produce a generation")
		}
		time.Sleep(time.Millisecond)
	}
	actual, err := backend.inspect(ctx, nameFor(request.Key))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(actual)
	if bytes.Contains(content, []byte("provider-fixture-secret")) {
		t.Fatal("provider credentials entered inventory")
	}
	captureRequest := execution.CaptureRequest{Workspace: request.Workspace, OperationID: "40000000-0000-4000-8000-000000000001", Purpose: execution.PublicationCapture}
	var captured []byte
	if err := backend.ReadWorkspace(ctx, captureRequest, func(input io.Reader) error { var err error; captured, err = io.ReadAll(input); return err }); err != nil {
		t.Fatal(err)
	}
	verifyCaptureFixture(t, captured)
	close(release)
	if line, err := reader.ReadString('\n'); err != nil || line != "mcp-ok\n" {
		t.Fatalf("MCP after capture=%q,%v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "{\"completed\":true}\n" {
		t.Fatalf("prompt after capture=%q,%v", line, err)
	}
	if err := process.Stop(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	captureRequest.Purpose = execution.TerminalCapture
	if err := backend.ReadWorkspace(ctx, captureRequest, func(input io.Reader) error { _, err := io.Copy(io.Discard, input); return err }); err != nil {
		t.Fatal(err)
	}
	if err := process.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	verifyCaptureFixture(t, captured)
	source.initialized = true
	ref.SessionID = ""
	source.ref = ref
	var archive bytes.Buffer
	if err := states.Export(ctx, ref, &archive); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive.Bytes(), []byte("provider-fixture-secret")) {
		t.Fatal("provider credentials entered Session archive")
	}
	if err := os.RemoveAll(backend.location(request.Key, "state")); err != nil {
		t.Fatal(err)
	}
	if err := states.Inspect(ctx, ref); !errors.Is(err, execution.ErrStateUnavailable) {
		t.Fatalf("missing protected state=%v", err)
	}
	if err := states.Restore(ctx, ref, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	request.Key.ExecutionEpoch++
	request.Workspace.Key = request.Key
	request.State.SessionID = request.Key.AgentSessionID
	request.Command = []string{"/bin/sh", "-ec", `test "$(cat /state/session.json)" = '{"session":"retained"}'; test ! -f /run/omnigrex/previous-turn; printf '{"continued":true}\n'; sleep 300`}
	second := launchFixture(t, ctx, backend, request)
	if line, err := bufio.NewReader(second.Transport()).ReadString('\n'); err != nil || line != "{\"continued\":true}\n" {
		t.Fatalf("continued state=%q,%v", line, err)
	}
	if err := second.Stop(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := second.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS: authenticated outstanding call, consistent capture, executable modes and symlinks, fresh terminal capture, retained state archive/restore, fresh VM isolation")
}

func strconvPort(address net.Addr) string {
	_, port, _ := net.SplitHostPort(address.String())
	return port
}
func sdkPlatformForOptions(t *testing.T, options Options) platform {
	t.Helper()
	driver, err := newPlatform(options)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}
func verifyCaptureFixture(t *testing.T, content []byte) {
	t.Helper()
	archive := tar.NewReader(bytes.NewReader(content))
	seen := map[string]bool{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(header.Name, "workspace/")
		switch name {
		case "generation":
			data, err := io.ReadAll(archive)
			if err != nil || len(data) != 1048576 || !bytes.Equal(data, bytes.Repeat([]byte("X"), 1048576)) {
				t.Fatal("generation is torn")
			}
			seen[name] = true
		case "executable":
			if header.Mode&0111 == 0 {
				t.Fatal("executable mode lost")
			}
			seen[name] = true
		case "link":
			if header.Typeflag != tar.TypeSymlink || header.Linkname != "executable" {
				t.Fatal("symlink lost")
			}
			seen[name] = true
		}
	}
	for _, name := range []string{"generation", "executable", "link"} {
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
}

type stateFixtureSource struct {
	ref         execution.StateRef
	initialized bool
}

func (source *stateFixtureSource) StateMetadata(_ context.Context, ref execution.StateRef) (json.RawMessage, bool, error) {
	actual := source.ref
	actual.SessionID = ref.SessionID
	if actual != ref {
		return nil, false, execution.ErrBindingMismatch
	}
	content, _ := json.Marshal(map[string]any{"version": 1, "subpath": "assignment-" + ref.ParticipantID + "/runtime-state"})
	return content, source.initialized, nil
}
