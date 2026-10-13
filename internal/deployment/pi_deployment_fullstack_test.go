package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual disposable gate across controlled process/HTTP boundaries.
// Live deployment qualification remains a separate Docker/KVM gate.
func TestPiDeploymentFullStackFailureBarriers(t *testing.T) {
	for _, mode := range []string{"success", "duplicate-event", "invalid-signature", "lost-storage", "reachable-postgres", "readiness-still-healthy"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			docker := `#!/bin/sh
root="${0%/*}"
printf '%s|%s|%s\n' "$COMPOSE_PROJECT_NAME" "$COMPOSE_FILE" "$*" >> "$root/calls"
case "$1 $2" in
  'image inspect') printf 'amd64\n';;
  'compose port') if [ -f "$root/recreated" ]; then printf '127.0.0.1:18081\n'; else printf '127.0.0.1:18080\n'; fi;;
  'compose up') case "$*" in *'--force-recreate'*orchestrator*) touch "$root/recreated";; esac;;
  'compose exec')
    case "$*" in
      *' doctor '*)
        for check in runtime-profile-contract runtime-profile-image pi-runtime postgres-connectivity postgres-schema protected-runtime-profiles pending-runtime-profile-compatibility mcp-endpoint docker-resources; do printf 'PASS %s\n' "$check"; done;;
      *"delivery.status || ':'"*) if [ MODE = duplicate-event ]; then printf 'PROCESSED:2\n'; else printf 'PROCESSED:1\n'; fi;;
      *"SELECT status"*323e*) printf 'IGNORED\n';;
      *"SELECT status"*123e*) printf 'PROCESSED\n';;
      *"SELECT count(*)"*) printf '0\n';;
      *'SELECT value FROM deployment_marker'*) printf 'persistent-ledger\n';;
      *'test "$(cat'*'.pi-deployment-marker'*) if [ MODE = lost-storage ]; then exit 42; fi;;
    esac;;
  'compose ps') printf 'disposable-postgres-id\n';;
  'inspect --format') printf '172.30.0.2\n';;
  'compose stop') touch "$root/stopped";;
  'compose start') rm -f "$root/stopped";;
  'run --rm') if [ MODE = reachable-postgres ]; then exit 42; fi;;
esac
`
			curl := `#!/bin/sh
root="${0%/*}"
printf '%s\n' "$*" >> "$root/http"
case "$*" in
  *'webhooks/github'*)
    case "$*" in
      *223e*) if [ MODE = invalid-signature ]; then printf 202; else printf 401; fi;;
      *) printf 202;;
    esac;;
  *readyz*)
    if [ -f "$root/stopped" ]; then
      if [ MODE = readiness-still-healthy ]; then printf 200; else printf 503; fi
    fi;;
  *healthz*) case "$*" in *http_code*) printf 200;; esac;;
esac
if [ -f "$root/recreated" ]; then
  case "$*" in *18080*) exit 43;; esac
fi
`
			for name, script := range map[string]string{"docker": docker, "curl": curl, "sleep": "#!/bin/sh\nexit 0\n"} {
				script = strings.ReplaceAll(script, "MODE", mode)
				if err := os.WriteFile(filepath.Join(root, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
			command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-deployment.sh")
			command.Dir = deployTestRepositoryRoot(t)
			command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
				"OMNIGREX_PI_BACKEND=docker", "OMNIGREX_PI_CUTOVER_REHEARSAL=0", "COMPOSE_PROJECT_NAME=production",
				"OMNIGREX_PI_DEPLOY_IMAGE="+image, "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE="+image)
			output, err := command.CombinedOutput()
			if mode == "success" {
				if err != nil {
					t.Fatalf("disposable full-stack gate failed: %v\n%s", err, output)
				}
				if !strings.Contains(string(output), "PASS durable authenticated webhooks") {
					t.Fatal("full-stack coverage did not complete")
				}
			} else if err == nil || strings.Contains(string(output), "PASS image-only Pi deployment") {
				t.Fatalf("%s produced false deployment success: %s", mode, output)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(calls), "\n") {
				if strings.Contains(line, "|compose ") && !strings.HasPrefix(line, "pi-deploy-") {
					t.Fatalf("touched ambient deployment: %s", line)
				}
			}
			if !strings.Contains(string(calls), "|compose.pi.yaml:compose.pi-docker.yaml:smoke.yaml|compose down --volumes --remove-orphans --timeout 10") {
				t.Fatal("fixture cleanup was not attempted")
			}
		})
	}
}
