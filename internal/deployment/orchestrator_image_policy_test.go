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
	for _, forbidden := range []string{"orchestrator-apk-packages", "--allow-untrusted", "--no-network"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("Dockerfile must not retain exact-APK closure fragment %q", forbidden)
		}
	}
}
