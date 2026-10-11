package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiCutoverStopsBeforeResetOnBackupFailure(t *testing.T) {
	calls := runFailedCutover(t, "backup")
	if !strings.Contains(calls, "pg_dump") {
		t.Fatal("rehearsal did not attempt a pre-reset backup")
	}
	// The failure trap may remove the disposable fixture, but the procedure
	// itself must not reset the installation after a failed recovery export.
	if strings.Contains(calls, "down --volumes --remove-orphans --timeout 60") {
		t.Fatal("cutover reset proceeded without a valid backup")
	}
}

func TestPiCutoverRejectsSurvivingLegacyVolume(t *testing.T) {
	calls := runFailedCutover(t, "surviving-volume")
	if !strings.Contains(calls, "down --volumes --remove-orphans --timeout 60") || !strings.Contains(calls, "volume ls --filter name=pi-deploy-") {
		t.Fatal("rehearsal did not verify explicit legacy disposal")
	}
	if strings.Contains(calls, "compose.pi.yaml:compose.pi-docker.yaml:smoke.yaml|compose up") {
		t.Fatal("Pi started while legacy storage survived")
	}
}

func TestPiCutoverRejectsUnknownStopInventory(t *testing.T) {
	calls := runFailedCutover(t, "inventory")
	if !strings.Contains(calls, "ps --quiet") || strings.Contains(calls, "pg_dump") {
		t.Fatal("unavailable stop inventory was treated as empty before backup")
	}
}

func TestPiCutoverRejectsUnknownVolumeInventory(t *testing.T) {
	calls := runFailedCutover(t, "volume-inventory")
	if !strings.Contains(calls, "volume ls --filter name=pi-deploy-") {
		t.Fatal("rehearsal did not attempt post-reset storage inventory")
	}
	if strings.Contains(calls, "compose.pi.yaml:compose.pi-docker.yaml:smoke.yaml|compose up") {
		t.Fatal("Pi started without confirmed legacy volume absence")
	}
}

func TestPiCutoverImageSelectionsReachBackup(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		for _, sources := range []string{"published", "built"} {
			for _, targets := range []string{"published", "built"} {
				t.Run(backend+"/"+sources+"-source/"+targets+"-target", func(t *testing.T) {
					overrides := []string{"OMNIGREX_PI_BACKEND=" + backend, "OMNIGREX_MICROSANDBOX_KVM_GID=1"}
					if sources == "built" {
						overrides = append(overrides, "OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE=", "OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE=")
					}
					if targets == "built" {
						overrides = append(overrides, "OMNIGREX_PI_DEPLOY_IMAGE=", "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE=")
					}
					calls := runFailedCutover(t, "backup", overrides...)
					if !strings.Contains(calls, "pg_dump") {
						t.Fatal("image selection failed before reaching the backup gate")
					}
					if sources == "built" || targets == "built" && backend == "docker" {
						if strings.Count(calls, "--network host") != 1 {
							t.Fatal("fixture publication requires exactly one initialized registry")
						}
					}
				})
			}
		}
	}
}

func runFailedCutover(t *testing.T, mode string, overrides ...string) string {
	t.Helper()
	root := t.TempDir()
	stub := `#!/bin/sh
printf '%s|%s|%s\n' "$COMPOSE_PROJECT_NAME" "$COMPOSE_FILE" "$*" >> "${0%/*}/calls"
case "$1 $2" in
  'image inspect')
    case "$*" in
      *RepoDigests*) printf 'registry.example/fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';;
      *) printf 'arm64\n';;
    esac;;
  'ps --quiet') if [ "$FAILURE_MODE" = inventory ]; then exit 42; fi;;
  'volume ls')
    if [ "$FAILURE_MODE" = volume-inventory ]; then exit 42; fi
    printf 'legacy-survivor\n';;
  'compose exec')
    case "$*" in
      *pg_dump*) if [ "$FAILURE_MODE" = backup ]; then exit 42; fi;;
    esac;;
  'run --rm')
    case "$*" in
      *'tar -czf'*) tar -czf - -T /dev/null;;
    esac;;
esac
exit 0
`
	// compose() intentionally strips ambient configuration, so persist the
	// fault selection in the stub rather than relying on forwarded environment.
	stub = strings.ReplaceAll(stub, `"$FAILURE_MODE"`, `"`+mode+`"`)
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
	command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-deployment.sh")
	command.Dir = deployTestRepositoryRoot(t)
	command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
		"OMNIGREX_PI_BACKEND=docker", "OMNIGREX_PI_CUTOVER_REHEARSAL=1", "COMPOSE_PROJECT_NAME=production",
		"OMNIGREX_PI_DEPLOY_IMAGE="+image, "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE="+image,
		"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE="+image, "OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE="+image)
	command.Env = append(command.Env, overrides...)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("incomplete cutover reported success: %s", output)
	}
	content, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	calls := string(content)
	for _, line := range strings.Split(calls, "\n") {
		if strings.Contains(line, "|compose ") && !strings.HasPrefix(line, "pi-deploy-") {
			t.Fatalf("rehearsal touched an ambient project: %s", line)
		}
	}
	return calls
}
