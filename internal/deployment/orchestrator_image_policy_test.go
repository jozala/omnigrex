package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOrchestratorImageBuildPolicy(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "../..")
	content, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if _, err := os.Stat(filepath.Join(root, "deploy/orchestrator-apk-packages.sha256")); !errors.Is(err, os.ErrNotExist) {
		t.Error("deploy/orchestrator-apk-packages.sha256 must be removed once the Dockerfile stops consuming it")
	}
	if !strings.Contains(text, "FROM alpine:3.24@sha256:") {
		t.Error("Dockerfile must retain the digest-pinned Alpine base image")
	}
	if !strings.Contains(text, "apk add --no-cache") {
		t.Error("Dockerfile must install controller packages with signature-verified apk add --no-cache")
	}
	for _, top := range []string{"bash", "ca-certificates", "curl", "git", "jq", "gcc", "musl-dev", "openssh-client-default", "ripgrep"} {
		if !strings.Contains(text, top) {
			t.Errorf("Dockerfile missing required controller package %s", top)
		}
	}
	for _, forbidden := range []string{"orchestrator-apk-packages", "--no-network", "--allow-untrusted", "apk_arch", "-eq 38", "dl-cdn.alpinelinux.org"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Dockerfile must not retain exact-APK closure fragment %q", forbidden)
		}
	}
	for _, required := range []string{"/usr/share/omnigrex/apk-packages.txt", "apk info -v", "USER 10001:10001", `ENTRYPOINT ["/usr/local/bin/omnigrex"]`, "/var/lib/omnigrex/workspaces", "/var/lib/omnigrex/mise", "chown -R 10001:10001 /var/lib/omnigrex"} {
		if !strings.Contains(text, required) {
			t.Errorf("Dockerfile missing required fragment %q", required)
		}
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts/check-orchestrator-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	scriptText := string(script)
	for _, required := range []string{"apk info -v", "/usr/share/omnigrex/apk-packages.txt", "git-remote-https", "ca-certificates.crt", "10001"} {
		if !strings.Contains(scriptText, required) {
			t.Errorf("check-orchestrator-image.sh missing required check %q", required)
		}
	}
}
