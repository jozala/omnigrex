package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiRecoveryRequiresConfirmedStoppedExecutionBeforeWritingFixtures(t *testing.T) {
	for _, mode := range []string{"docker-unknown", "docker-active", "native-unknown", "native-active", "empty"} {
		t.Run(mode, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls")
			script := `
. "$HELPER"
namespace=pi-recovery-fixture
backend=microsandbox
pi=unused-image
docker() {
  printf 'docker %s\n' "$*" >> "$CALLS"
  case "$MODE" in
    docker-unknown) return 42;;
    docker-active) printf 'active-environment\n';;
  esac
}
compose() {
  printf 'compose %s\n' "$*" >> "$CALLS"
  case "$*" in
    'stop orchestrator') return 0;;
    'run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet')
      case "$MODE" in
        native-unknown) return 42;;
        native-active) printf 'active-vm\n';;
      esac;;
    *) return 93;;
  esac
}
verify_pi_recovery
`
			command := exec.CommandContext(t.Context(), "sh", "-ec", script)
			command.Env = append(os.Environ(), "HELPER="+filepath.Join(deployTestRepositoryRoot(t), "scripts/pi-recovery-smoke.sh"), "CALLS="+calls, "MODE="+mode)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("controlled failure did not stop recovery: %s", output)
			}
			content, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			wrote := strings.Contains(string(content), "CREATE TABLE pi_recovery_marker")
			if wrote != (mode == "empty") {
				t.Fatalf("recovery write barrier (%s):\n%s", mode, content)
			}
			if strings.Contains(string(content), "pg_dump") || strings.Contains(string(content), "dst=/target") {
				t.Fatalf("controlled inventory check reached backup or restore:\n%s", content)
			}
		})
	}
}
