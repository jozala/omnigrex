//go:build integration

package deployment

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestQualificationContextKeepsWebsiteGuides(t *testing.T) {
	root := repositoryRoot(t)
	image := fmt.Sprintf("omnigrex/documentation-context-test:%d", time.Now().UnixNano())
	container := fmt.Sprintf("omnigrex-documentation-context-test-%d", time.Now().UnixNano())
	// Use the real repository Docker context and .dockerignore, but no runtime,
	// base-image download, or network access. Qualification images COPY the same
	// context before running the repository's documentation-link tests.
	dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\nCOPY website/ /website/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("docker", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, output)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "--force", container).Run()
		_ = exec.Command("docker", "image", "rm", image).Run()
	})
	run("build", "--file", dockerfile, "--tag", image, ".")
	// A stopped scratch container is only used to inspect the copied files.
	run("create", "--name", container, image, "/not-executed")
	destination := t.TempDir()
	run("cp", container+":/website/.", destination)
	for _, name := range []string{"README.md", "DEPLOYMENT.md"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Errorf("qualification context is missing linked website guide %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			t.Errorf("qualification context includes website build input %s; only maintainer Markdown should be copied", entry.Name())
		}
	}
}
