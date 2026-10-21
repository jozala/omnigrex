package dockerbuild_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runWrapper(t *testing.T, cacheOnly, cacheDir, localImage, pushImage string, failure bool, args ...string) ([]string, error) {
	t.Helper()
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS\"\nexit \"$DOCKER_EXIT\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "args")
	cmd := exec.Command("sh", append([]string{"../docker-build.sh"}, args...)...)
	exit := "0"
	if failure {
		exit = "37"
	}
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOCKER_ARGS="+log,
		"DOCKER_EXIT="+exit,
		"OMNIGREX_BUILDKIT_CACHE_DIR="+cacheDir,
		"OMNIGREX_BUILDKIT_CACHE_ONLY="+cacheOnly,
		"OMNIGREX_BUILDKIT_PUSH_IMAGE="+pushImage,
		"OMNIGREX_BUILDKIT_LOCAL_IMAGE="+localImage,
	)
	err := cmd.Run()
	data, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), err
}

func TestLocalBuildPreservesArgumentsAndFailure(t *testing.T) {
	args := []string{"--file", "a b.Dockerfile", "--build-arg", "VALUE=a b", "--tag", "example:test", "."}
	got, err := runWrapper(t, "0", "", "", "", true, args...)
	if err == nil {
		t.Fatal("expected Docker failure to propagate")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("exit status = %v, want 37", err)
	}
	want := append([]string{"build"}, args...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Docker arguments = %q, want %q", got, want)
	}
}

func TestCachedBuildScopesTargetsAndPreservesArguments(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	for _, tc := range []struct {
		args []string
		tag  string
	}{
		{[]string{"--file", "agent/pi/Dockerfile", "--target", "golden", "--build-arg", "VALUE=a b", "."}, "agent-pi-Dockerfile-golden"},
		{[]string{"-f", "agent/pi/Dockerfile", "--target=upgrade-source", "."}, "agent-pi-Dockerfile-upgrade-source"},
	} {
		got, err := runWrapper(t, "0", cache, "", "", false, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{
			"buildx", "build", "--output", "type=docker",
			"--cache-from", "type=local,src=" + cache + ",tag=" + tc.tag,
			"--cache-to", "type=local,dest=" + cache + ",tag=" + tc.tag + ",mode=max,reset=true,ignore-error=true",
		}, tc.args...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Docker arguments = %q, want %q", got, want)
		}
	}
}

func TestCacheOnlyBuildAndFailure(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	got, err := runWrapper(t, "1", cache, "", "", true, "--file=qualified.Dockerfile", "--target", "test", ".")
	if err == nil {
		t.Fatal("expected Docker failure to propagate")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("exit status = %v, want 37", err)
	}
	if !reflect.DeepEqual(got[:4], []string{"buildx", "build", "--output", "type=cacheonly"}) {
		t.Fatalf("cache-only Docker arguments = %q", got)
	}
	if !strings.Contains(strings.Join(got, " "), "tag=qualified-Dockerfile-test") {
		t.Fatalf("missing cache-only target tag in %q", got)
	}
	got, err = runWrapper(t, "1", "", "", "", false, ".")
	if err != nil || !reflect.DeepEqual(got, []string{"buildx", "build", "--output", "type=cacheonly", "."}) {
		t.Fatalf("local cache-only build = %q, %v", got, err)
	}
}

func TestCachedBuildCanLoadAndPublishSameResult(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	image := "omnigrex-ci-registry:5000/omnigrex/pi:test"
	got, err := runWrapper(t, "0", cache, "omnigrex/pi:test", image, false, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"buildx", "build", "--output", "type=docker,name=omnigrex/pi:test", "--cache-from", "type=local,src=" + cache + ",tag=Dockerfile-default", "--cache-to", "type=local,dest=" + cache + ",tag=Dockerfile-default,mode=max,reset=true,ignore-error=true", "--output", "type=image,name=" + image + ",push=true,registry.insecure=true", "."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Docker arguments = %q, want %q", got, want)
	}
}
