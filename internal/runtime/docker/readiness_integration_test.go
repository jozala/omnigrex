//go:build integration

package docker_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestReadinessProbeExercisesRealDockerResources(t *testing.T) {
	image := environmentOrDefault("OMNIGREX_PI_IMAGE", "omnigrex/pi:1.4.0")
	image = readinessRegistryImage(t, image)
	arch := strings.TrimSpace(dockerCommand(t, "image", "inspect", image, "--format", "{{.Architecture}}"))
	bound, err := profile.NewPiV2(image, profile.Platform{OS: "linux", Arch: arch})
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	network := "omnigrex-readiness-agent-" + suffix
	workspace := "omnigrex-readiness-workspaces-" + suffix
	runtimeState := "omnigrex-readiness-runtime-state-" + suffix
	mise := "omnigrex-readiness-mise-" + suffix

	dockerCommand(t, "network", "create", network)
	t.Cleanup(func() { dockerCleanup(t, "network", "rm", network) })
	for _, volume := range []string{workspace, runtimeState, mise} {
		dockerCommand(t, "volume", "create", volume)
		volume := volume
		t.Cleanup(func() { dockerCleanup(t, "volume", "rm", "--force", volume) })
	}
	initializeVolumeRoots(t, image, workspace, runtimeState, mise)

	probe, err := dockerruntime.NewReadinessProbe(dockerruntime.ReadinessProbeOptions{
		AgentNetwork:       network,
		WorkspaceVolume:    workspace,
		RuntimeStateVolume: runtimeState,
		MiseVolume:         mise,
		AgentImage:         image,
		RuntimeProfile:     bound,
	})
	if err != nil {
		t.Fatalf("NewReadinessProbe() error = %v", err)
	}
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("close readiness probe: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := probe.Check(ctx); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	assertReadinessCleanup(t, image, workspace, runtimeState, mise)
}

func readinessRegistryImage(t *testing.T, image string) string {
	t.Helper()
	if profile.IsExactRegistryImage(image) {
		return image
	}
	name := fmt.Sprintf("omnigrex-readiness-registry-%d", time.Now().UnixNano())
	// Bind inside the daemon's network namespace, including Docker Desktop's VM.
	dockerCommand(t, "run", "--detach", "--name", name, "--network", "host", "--env", "REGISTRY_HTTP_ADDR=127.0.0.1:0", "registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373")
	t.Cleanup(func() { dockerCleanup(t, "rm", "--force", "--volumes", name) })
	var address string
	listener := regexp.MustCompile(`listening on (127\.0\.0\.1:[0-9]+)`)
	for attempt := 0; ; attempt++ {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if match := listener.FindStringSubmatch(string(logs)); len(match) == 2 {
			address = match[1]
			if err := exec.Command("docker", "exec", name, "wget", "-q", "-O", "/dev/null", "http://"+address+"/v2/").Run(); err == nil {
				break
			}
		}
		if attempt >= 30 {
			t.Fatal("readiness registry did not become available")
		}
		time.Sleep(100 * time.Millisecond)
	}
	tag := address + "/omnigrex/pi:readiness"
	dockerCommand(t, "tag", image, tag)
	t.Cleanup(func() { dockerCleanup(t, "image", "rm", tag) })
	dockerCommand(t, "push", tag)
	return strings.TrimSpace(dockerCommand(t, "image", "inspect", tag, "--format", "{{index .RepoDigests 0}}"))
}

func initializeVolumeRoots(t *testing.T, image, workspace, runtimeState, mise string) {
	t.Helper()
	dockerCommand(t,
		"run", "--rm",
		"--user", "10001:10001",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", "none",
		"--mount", "type=volume,src="+workspace+",dst=/workspace",
		"--mount", "type=volume,src="+runtimeState+",dst=/state",
		"--mount", "type=volume,src="+mise+",dst=/home/omnigrex/.local/share/mise",
		"--entrypoint", "sh",
		image,
		"-c", "for path in /workspace /state /home/omnigrex/.local/share/mise; do touch \"$path/.volume-root-probe\" && rm \"$path/.volume-root-probe\"; done",
	)
}

func assertReadinessCleanup(t *testing.T, image, workspace, runtimeState, mise string) {
	t.Helper()
	dockerCommand(t,
		"run", "--rm",
		"--user", "10001:10001",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", "none",
		"--mount", "type=volume,src="+workspace+",dst=/workspace",
		"--mount", "type=volume,src="+runtimeState+",dst=/state",
		"--mount", "type=volume,src="+mise+",dst=/home/omnigrex/.local/share/mise",
		"--entrypoint", "sh",
		image,
		"-c", "for root in /workspace /state /home/omnigrex/.local/share/mise; do set -- \"$root\"/readiness-*; test ! -e \"$1\"; done",
	)
}

func dockerCommand(t *testing.T, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func dockerCleanup(t *testing.T, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput(); err != nil {
		t.Errorf("docker cleanup %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func environmentOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
