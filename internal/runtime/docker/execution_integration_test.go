//go:build integration

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
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
	conformance.Transport(t, ctx, factory, request, newExecutionTestLedger())
}

func TestExecutionPiUsesNeutralFactoryAndTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "omnigrex/pi:1.0.4", "--format", "{{.Id}} {{.Architecture}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		t.Fatal("fixture image identity is incomplete")
	}
	profile, err := runtimeprofile.NewPiV2("registry.example/omnigrex/pi@"+fields[0], runtimeprofile.Platform{OS: "linux", Arch: fields[1]})
	if err != nil {
		t.Fatal(err)
	}
	key := executionTestRequest().Key
	key.AgentTurnID = fmt.Sprintf("30000000-0000-4000-8000-%012x", time.Now().UnixNano()&((1<<48)-1))
	request, err := (pi.TurnAdapter{}).BuildProcess(profile, key, "", 512<<20, nil)
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
	stopRead := context.AfterFunc(ctx, func() { _ = process.Transport().Close() })
	defer stopRead()
	// The runner fails closed on invalid bootstrap input over the neutral streams.
	// Full Pi protocol and Session coverage lives in the shared both-backend Pi suite.
	if _, err := io.WriteString(process.Transport(), "{}\n"); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(process.Transport())
	if err != nil || !strings.Contains(string(response), `"type":"omnigrex_configuration_error"`) {
		t.Fatalf("neutral Pi bootstrap rejection = %q, %v", response, err)
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
