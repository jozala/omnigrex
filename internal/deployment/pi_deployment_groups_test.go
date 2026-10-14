package deployment_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPiDeploymentSecretsUseDistinctBackendGroup(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		for _, gid := range []int{os.Getgid(), 10001} {
			t.Run(fmt.Sprintf("%s/%d", backend, gid), func(t *testing.T) {
				root := t.TempDir()
				stub := `#!/bin/sh
root="${0%/*}"
case "$1 $2" in
  'image inspect') printf 'amd64\n';;
  'run --rm')
    case "$*" in *'dst=/secrets'*) printf '%s\n' "$*" > "$root/secret-init";; esac;;
  'compose run'|'compose up') cp .env "$root/fixture.env"; exit 42;;
esac
`
				for name, script := range map[string]string{
					"docker": stub,
					"stat":   fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%d'\n", gid),
				} {
					if err := os.WriteFile(filepath.Join(root, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
				command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-deployment.sh")
				command.Dir = deployTestRepositoryRoot(t)
				command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
					"OMNIGREX_PI_BACKEND="+backend, "OMNIGREX_PI_CUTOVER_REHEARSAL=0", "OMNIGREX_MICROSANDBOX_KVM_GID="+strconv.Itoa(gid),
					"OMNIGREX_PI_DEPLOY_IMAGE="+image, "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE="+image)
				if output, err := command.CombinedOutput(); err == nil {
					t.Fatalf("fixture did not stop at the controlled deployment boundary: %s", output)
				}
				content, err := os.ReadFile(filepath.Join(root, "fixture.env"))
				if err != nil {
					t.Fatal(err)
				}
				values := make(map[string]string)
				for line := range strings.SplitSeq(string(content), "\n") {
					key, value, _ := strings.Cut(line, "=")
					values[key] = value
				}
				secret := values["OMNIGREX_SECRET_GID"]
				if _, err := strconv.ParseUint(secret, 10, 32); err != nil {
					t.Fatalf("invalid secret group %q: %v", secret, err)
				}
				if secret == values["OMNIGREX_DOCKER_GID"] || secret == values["OMNIGREX_MICROSANDBOX_KVM_GID"] {
					t.Fatalf("secret group %s collides with backend access group", secret)
				}
				initialization, err := os.ReadFile(filepath.Join(root, "secret-init"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasSuffix(strings.TrimSpace(string(initialization)), "sh "+secret) {
					t.Fatalf("secret files were not assigned the selected group: %s", initialization)
				}
			})
		}
	}
}
