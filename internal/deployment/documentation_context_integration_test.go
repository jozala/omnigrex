//go:build integration

package deployment

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestQualificationContextKeepsWebsiteFixtures(t *testing.T) {
	root := repositoryRoot(t)
	image := fmt.Sprintf("omnigrex/documentation-context-test:%d", time.Now().UnixNano())
	container := fmt.Sprintf("omnigrex-documentation-context-test-%d", time.Now().UnixNano())
	// Use the real repository Docker context and .dockerignore, but no runtime,
	// base-image download, or network access. Qualification images COPY the same
	// context before running documentation-link and deployment integration tests.
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
	for _, name := range []string{"README.md", "DEPLOYMENT.md", "scripts/deploy.sh"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Errorf("qualification context is missing website fixture %s: %v", name, err)
		}
	}
	if err := filepath.WalkDir(destination, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(destination, path)
		if err != nil {
			return err
		}
		if name == "." || (entry.IsDir() && name == "scripts") ||
			(!entry.IsDir() && (name == filepath.Join("scripts", "deploy.sh") || (filepath.Dir(name) == "." && filepath.Ext(name) == ".md"))) {
			return nil
		}
		t.Errorf("qualification context includes website build input %s; only maintainer Markdown and the deployment script should be copied", name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
