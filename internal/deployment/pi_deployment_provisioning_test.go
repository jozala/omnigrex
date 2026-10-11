package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiPublishedNativeImageProvisioningPreservesRegistryIdentity(t *testing.T) {
	root := t.TempDir()
	stub := `#!/bin/sh
printf '%s\n' "$*" >> "${0%/*}/calls"
case "$1 $2" in
  'image inspect') printf 'amd64\n';;
  'compose run')
    case "$*" in
      *'image pull '*|*'image load '*) exit 42;;
    esac;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
	command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-deployment.sh")
	command.Dir = deployTestRepositoryRoot(t)
	command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
		"OMNIGREX_PI_BACKEND=microsandbox", "OMNIGREX_MICROSANDBOX_KVM_GID=1",
		"OMNIGREX_PI_DEPLOY_IMAGE="+image, "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE=registry.example/orchestrator@sha256:"+strings.Repeat("b", 64))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("failed registry provisioning reported success: %s", output)
	}
	content, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	calls := string(content)
	if !strings.Contains(calls, "orchestrator image pull "+image+"\n") {
		t.Fatalf("published digest was not provisioned from its registry: %s", calls)
	}
	for _, forbidden := range []string{"image save", "image load", "compose up"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("failed published-image provisioning performed %q: %s", forbidden, calls)
		}
	}
}

func deployTestRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
