package deployment_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiLiveSmokeStreamsCredentialsOnlyIntoDisposableTmpfs(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			auth := filepath.Join(root, "private credentials.json")
			credentials := []byte(`{"openai":{"type":"api_key","key":"fixture-live-secret-never-in-arguments"}}`)
			if err := os.WriteFile(auth, credentials, 0600); err != nil {
				t.Fatal(err)
			}
			calls, delivered := filepath.Join(root, "calls"), filepath.Join(root, "delivered")
			stub := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
  'image inspect '*"{{.Id}}"*) printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';;
  'image inspect '*"{{.Architecture}}"*) printf 'amd64\n';;
  'run '*'-c %g /var/run/docker.sock') printf '123\n';;
  'image save --output '*) : > "$4";;
  'start --attach --interactive '*) cat > "$DELIVERED";;
  'inspect '*"{{.State.ExitCode}}"*) printf '0\n';;
esac
`
			if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "sh", filepath.Join(deployTestRepositoryRoot(t), "scripts/test-pi-live.sh"))
			command.Dir = deployTestRepositoryRoot(t)
			command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "CALLS="+calls, "DELIVERED="+delivered, "OMNIGREX_PI_LIVE_AUTH_FILE="+auth, "OMNIGREX_PI_BACKEND="+backend, "OMNIGREX_MICROSANDBOX_KVM_GID=123")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("live smoke setup: %v\n%s", err, output)
			}
			metadata, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.ReadFile(delivered)
			if err != nil {
				t.Fatal(err)
			}
			unchanged, err := os.ReadFile(auth)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(input, credentials) || !bytes.Equal(unchanged, credentials) {
				t.Fatal("smoke changed credentials or failed to stream their exact bytes")
			}
			if bytes.Contains(metadata, []byte("fixture-live-secret-never-in-arguments")) || bytes.Contains(output, []byte("fixture-live-secret-never-in-arguments")) || bytes.Contains(metadata, []byte(auth)) {
				t.Fatal("credentials entered backend metadata or output")
			}
			if !strings.Contains(string(metadata), "--tmpfs /run/pi-live:rw,noexec,nosuid,nodev") || !strings.Contains(string(metadata), "cat > /run/pi-live/auth.json") {
				t.Fatal("smoke did not isolate incoming credentials in tmpfs")
			}
		})
	}
}
