//go:build integration

package docker_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestOpenCodeImageRunsGoRaceTests(t *testing.T) {
	image := environmentOrDefault("OMNIGREX_OPENCODE_IMAGE", "omnigrex/opencode:1.18.29")
	arch := dockerCommand(t, "image", "inspect", "--format", "{{.Architecture}}", image)
	profile, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		runtimeprofile.Platform{OS: "linux", Arch: arch},
	)
	if err != nil {
		t.Fatal(err)
	}
	contract := profile.Contract()
	module, err := os.ReadFile("../../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	var goVersion string
	for _, line := range strings.Split(string(module), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			goVersion = fields[1]
			break
		}
	}
	if goVersion == "" {
		t.Fatal("repository go.mod has no pinned Go version")
	}
	fixture := t.TempDir()
	if err := os.Chmod(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":       "module example.invalid/racefixture\n\ngo " + goVersion + "\n",
		"race_test.go": goRaceFixture,
	} {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prefix := fmt.Sprintf("omnigrex-go-race-%d", time.Now().UnixNano())
	setup := []string{"run", "--platform", "linux/" + arch, "--user", "0:0", "--network", "none",
		"--mount", "type=bind,src=" + fixture + ",dst=/fixture,readonly"}
	run := []string{"run", "--platform", "linux/" + arch,
		"--user", fmt.Sprintf("%d:%d", contract.User.UID, contract.User.GID),
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "512m", "--pids-limit", fmt.Sprint(contract.PIDsLimit), "--workdir", contract.Workspace}
	for _, mount := range contract.Mounts {
		volume := prefix + "-" + mount.Name
		dockerCommand(t, "volume", "create", volume)
		t.Cleanup(func() { dockerCleanup(t, "volume", "rm", "--force", volume) })
		setup = append(setup, "--mount", "type=volume,src="+volume+",dst=/volumes/"+mount.Name)
		run = append(run, "--mount", "type=volume,src="+volume+",dst="+mount.Path+",volume-subpath=assignment")
	}
	setupName := prefix + "-setup"
	t.Cleanup(func() { dockerCleanup(t, "container", "rm", "--force", setupName) })
	// Keep setup named until cleanup so a Docker client timeout cannot leave
	// an unaddressable container holding the test volumes open.
	setup = append(setup, "--name", setupName)
	dockerCommand(t, append(setup, "--entrypoint", "sh", image, "-ec", `
for root in /volumes/*; do
  mkdir -p "$root/assignment"
done
cp /fixture/* /volumes/workspace/assignment/
mkdir -p /volumes/tool-data/assignment/turn/build-tmp \
  /volumes/tool-data/assignment/assignment/go-build-cache \
  /volumes/tool-data/assignment/assignment/go-modules
chown -R 10001:10001 /volumes/*/assignment
`)...)
	for _, temporary := range contract.Tmpfs {
		options := fmt.Sprintf("rw,nosuid,nodev,size=%d,uid=%d,gid=%d,mode=0700", temporary.SizeBytes, contract.User.UID, contract.User.GID)
		if !temporary.Executable {
			options += ",noexec"
		}
		run = append(run, "--tmpfs", temporary.Path+":"+options)
	}
	for _, variable := range contract.Environment {
		run = append(run, "--env", variable.Name+"="+variable.Value)
	}
	for _, variable := range []string{
		"TMPDIR=" + workspace.TurnPathMount + "/turn/build-tmp",
		"GOTMPDIR=" + workspace.TurnPathMount + "/turn/build-tmp",
		"GOCACHE=" + workspace.TurnPathMount + "/assignment/go-build-cache",
		"GOPATH=" + workspace.TurnPathMount + "/assignment/go-modules",
		"GOTOOLCHAIN=local",
	} {
		run = append(run, "--env", variable)
	}
	// Bound the combined phases below the outer 30-minute package timeout so
	// normal failures and cancellation still leave time for t.Cleanup.
	testCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	runContainer := func(step, network, script string) (string, error) {
		t.Helper()
		name := prefix + "-" + step
		t.Cleanup(func() { dockerCleanup(t, "container", "rm", "--force", name) })
		// Tool downloads and a cold race-instrumented standard library build can
		// exceed the short timeout used by ordinary Docker resource probes.
		ctx, cancel := context.WithTimeout(testCtx, 10*time.Minute)
		defer cancel()
		arguments := append([]string(nil), run...)
		arguments = append(arguments, "--name", name, "--network", network, "--entrypoint", "sh",
			image, "-ec", script, "sh", "go@"+goVersion)
		output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
		if ctx.Err() != nil {
			return string(output), ctx.Err()
		}
		return string(output), err
	}
	if output, err := runContainer("install", "bridge", `mise install "$1"`); err != nil {
		t.Fatalf("install pinned Go toolchain: %v\n%s", err, output)
	}
	output, err := runContainer("safe", "none", `mise exec "$1" -- sh -ec '
test "$(id -u)" = 10001
test "$(id -g)" = 10001
go env CGO_ENABLED CC
test "$(go env CGO_ENABLED)" = 1
go test -p 1 -race -count=1 -run "^TestSynchronized$" -v .
'`)
	if err != nil {
		t.Fatalf("run race-enabled test as hardened Runtime Process: %v\n%s", err, output)
	}
	if !strings.Contains(output, "--- PASS: TestSynchronized") {
		t.Fatalf("synchronized test did not execute:\n%s", output)
	}
	output, err = runContainer("racy", "none", `mise exec "$1" -- go test -p 1 -race -count=1 -run '^TestIntentionalRace$' -v .`)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 ||
		!strings.Contains(output, "WARNING: DATA RACE") || !strings.Contains(output, "--- FAIL: TestIntentionalRace") {
		t.Fatalf("intentional race was not detected: %v\n%s", err, output)
	}
}

const goRaceFixture = `package racefixture

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSynchronized(t *testing.T) {
	var value atomic.Int64
	var done sync.WaitGroup
	done.Add(2)
	for range 2 {
		go func() {
			defer done.Done()
			for range 1000 {
				value.Add(1)
			}
		}()
	}
	done.Wait()
	if value.Load() != 2000 {
		t.Fatalf("value = %d, want 2000", value.Load())
	}
}

func TestIntentionalRace(t *testing.T) {
	var value int
	var done sync.WaitGroup
	start := make(chan struct{})
	done.Add(2)
	for range 2 {
		go func() {
			defer done.Done()
			<-start
			for range 1000 {
				value++
			}
		}()
	}
	close(start)
	done.Wait()
	t.Logf("value = %d", value)
}
`
