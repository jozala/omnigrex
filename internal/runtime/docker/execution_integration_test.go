//go:build integration

package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestExecutionConformanceDocker(t *testing.T) {
	const image = "alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
		t.Fatalf("pull fixture image: %v\n%s", err, output)
	}
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", image, "--format", "{{.Architecture}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	request := executionTestRequest()
	request.Key.AgentTurnID = fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
	request.Image, request.Platform.Architecture = image, strings.TrimSpace(string(output))
	request.WorkingDir = "/tmp"
	request.TemporaryDirectories = []execution.TemporaryDirectory{{Target: "/tmp", SizeBytes: 4 << 20}}
	request.Command = []string{"/bin/sh", "-c", `printf '{"ready":true}\n'; printf 'stderr-marker\n' >&2; sleep 300 & while IFS= read -r line; do printf '%s\n' "$line"; done; wait`}
	factory := ExecutionFactory{Network: "none"}
	runExecutionTransportConformance(t, ctx, factory, request, newExecutionTestLedger())
}

func TestExecutionOpenCodeUsesNeutralFactoryAndTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "omnigrex/opencode:1.18.29", "--format", "{{.Id}} {{.Architecture}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		t.Fatal("fixture image identity is incomplete")
	}
	profile, err := runtimeprofile.NewOpenCodeV1("registry.example/omnigrex/opencode@"+fields[0], runtimeprofile.Platform{OS: "linux", Arch: fields[1]})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{Instructions: "Use only the authorized Role capabilities.", Model: "openai/gpt-5", Steps: 1, Permissions: opencode.PermissionPolicy{"read": opencode.PermissionAllow}})
	if err != nil {
		t.Fatal(err)
	}
	key := executionTestRequest().Key
	key.AgentTurnID = fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
	request, err := opencode.BuildProcess(profile, rendered, opencode.ProviderCredentials{Role: opencode.RoleDeveloper, Content: []byte(`{}`)}, opencode.ProcessOptions{Key: key, MemoryBytes: 512 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// The local image ID is qualified by the fixture. This check deliberately uses
	// disposable Session storage while exercising the production request compiler.
	request.Image = fields[0]
	for _, attachment := range request.Attachments {
		request.TemporaryDirectories = append(request.TemporaryDirectories, execution.TemporaryDirectory{Target: attachment.Target, SizeBytes: 64 << 20})
	}
	request.Attachments = nil
	engine, err := (ExecutionFactory{Network: "none"}).New(request, newExecutionTestLedger())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := engine.EnsureAbsent(cleanupCtx); err != nil {
			t.Error(err)
		}
	}()
	process, err := engine.Create(ctx, request, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(ctx); err != nil {
		t.Fatal(err)
	}
	client := acp.NewClient(process.Transport(), acp.ClientOptions{RequiredCapabilities: acp.RequiredCapabilities{SessionList: true, SessionResume: true, SessionLoad: true}})
	defer client.Close()
	initialized, err := client.Initialize(ctx)
	if err != nil || initialized.AgentInfo == nil || initialized.AgentInfo.Name != "OpenCode" || initialized.AgentInfo.Version != "1.18.29" {
		t.Fatalf("neutral OpenCode initialization = %#v, %v", initialized.AgentInfo, err)
	}
	if _, err := client.CreateSession(ctx, acp.CreateSessionRequest{CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionKnownMissingImageHasNoUnresolvedCreation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request := executionTestRequest()
	request.Key.AgentTurnID = fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
	request.Image = "registry.invalid/omnigrex/missing@sha256:" + strings.Repeat("0", 64)
	ledger := newExecutionTestLedger()
	engine, err := (ExecutionFactory{Network: "none"}).New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	process, err := engine.Create(ctx, request, io.Discard)
	if process != nil || err == nil || errors.Is(err, execution.ErrUnknownEffect) {
		t.Fatalf("known missing image = %v, %v", process, err)
	}
	if err := engine.EnsureAbsent(ctx); err != nil {
		t.Fatalf("known no-effect creation cannot be cleaned: %v", err)
	}
	if !ledger.records[request.Key].CreationSettled || ledger.records[request.Key].Resource != "" {
		t.Fatal("missing image was recorded as an ambiguous resource")
	}
}

// The scenarios use only neutral contracts so a second backend can run the same
// assertions, independently of the Docker-specific factory and image acquisition.
func runExecutionTransportConformance(t *testing.T, ctx context.Context, factory execution.Factory, request execution.Request, ledger execution.Ledger) {
	t.Helper()
	engine, err := factory.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := engine.EnsureAbsent(cleanupCtx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	stderr := &executionTestOutput{}
	process, err := engine.Create(ctx, request, stderr)
	if err != nil {
		t.Fatal(err)
	}
	if observed, err := process.ObserveExit(ctx); err != nil || observed.Exited || observed.Running {
		t.Fatalf("unstarted exit evidence: %#v, %v", observed, err)
	}
	_ = process.Transport().Close()
	// Retry the same provision operation before launch, as after a lost attach response.
	retryEngine, err := factory.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer retryEngine.Close()
	retry, err := retryEngine.Create(ctx, request, stderr)
	if err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Command = []string{"/bin/sh", "-c", "exit 0"}
	conflicting, err := factory.New(changed, ledger)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conflicting.Create(ctx, changed, io.Discard)
	_ = conflicting.Close()
	if !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("changed operation = %v", err)
	}
	if err := retry.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(retry.Transport())
	readLine := func(phase string) string {
		t.Helper()
		type result struct {
			line string
			err  error
		}
		done := make(chan result, 1)
		go func() { line, err := reader.ReadString('\n'); done <- result{line, err} }()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			return got.line
		case <-ctx.Done():
			_ = retry.Transport().Close()
			t.Fatalf("%s transport did not complete within deadline", phase)
			return ""
		}
	}
	if got := readLine("startup"); got != "{\"ready\":true}\n" {
		t.Fatalf("startup stdout = %q", got)
	}
	if _, err := retry.Transport().Write([]byte("{\"id\":7}\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLine("round trip"); got != "{\"id\":7}\n" {
		t.Fatalf("round trip = %q", got)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(stderr.String(), "stderr-marker") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "stderr-marker") {
		t.Fatal("stderr was not delivered separately")
	}
	_ = retry.Transport().Close()
	if observed, err := retry.ObserveExit(ctx); err != nil || !observed.Running || observed.OOMKilled {
		t.Fatalf("detachment implied stop: %#v, %v", observed, err)
	}
	if err := retry.Stop(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	observed, err := retry.ObserveExit(ctx)
	if err != nil || !observed.Exited || observed.Running || observed.OOMKilled || observed.Resource == "" {
		t.Fatalf("confirmed environment stop = %#v, %v", observed, err)
	}
	if err := retry.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureAbsent(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := retry.ObserveExit(ctx); err != nil || got.Exited || got.OOMKilled {
		t.Fatalf("missing exit evidence = %#v, %v", got, err)
	}
	// All handles refer to the same resource and none may recreate this revoked epoch.
	if _, err := engine.Create(ctx, request, io.Discard); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("revoked epoch creation = %v", err)
	}
}

type executionTestOutput struct {
	mutex sync.Mutex
	value strings.Builder
}

func (output *executionTestOutput) Write(data []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.value.Write(data)
}
func (output *executionTestOutput) String() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.value.String()
}
