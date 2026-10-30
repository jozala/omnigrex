package workflows

import (
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReleaseBehavior(t *testing.T) {
	cmd := exec.Command("python3", "-m", "unittest", "-v", "test_release.py")
	cmd.Dir = filepath.Join("..", "..", "scripts", "release")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release behavior: %v\n%s", err, out)
	}
}

func TestReleaseWorkflowBoundaries(t *testing.T) {
	for _, channel := range []string{"alpha", "stable"} {
		b, err := os.ReadFile("release-" + channel + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		var w struct {
			On          map[string]interface{} `yaml:"on"`
			Permissions map[string]string      `yaml:"permissions"`
			Jobs        map[string]struct {
				If          string            `yaml:"if"`
				Permissions map[string]string `yaml:"permissions"`
				Steps       []ciStep          `yaml:"steps"`
			} `yaml:"jobs"`
			Concurrency struct {
				Group  string `yaml:"group"`
				Cancel bool   `yaml:"cancel-in-progress"`
			} `yaml:"concurrency"`
		}
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		if len(w.On) != 1 || w.On["workflow_dispatch"] == nil || w.Permissions["contents"] != "read" || w.Concurrency.Group != "product-release" || w.Concurrency.Cancel {
			t.Fatal("manual serialized releases required")
		}
		job := w.Jobs["release"]
		if job.If != "github.ref == 'refs/heads/main'" || !reflect.DeepEqual(job.Permissions, map[string]string{"contents": "write", "actions": "read"}) {
			t.Fatal("release authority changed")
		}
		if len(job.Steps) != 2 || job.Steps[1].Run != "python3 scripts/release/release.py "+channel+" release-assets" {
			t.Fatal("release must only designate existing artifacts")
		}
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "${{") {
				t.Fatal("dispatch input interpolated into shell")
			}
		}
	}
	jobs := readCIJobs(t)
	if got := ciNeeds(t, jobs["release-candidate"]); !reflect.DeepEqual(got, []string{"publish", "deploy-smoke", "deploy-smoke-microsandbox", "development-smoke-microsandbox"}) {
		t.Fatal(got)
	}
	for _, name := range []string{"deploy-smoke", "deploy-smoke-microsandbox"} {
		found := false
		for _, step := range jobs[name].Steps {
			if strings.Contains(step.Run, "test-pi-deployment.sh") {
				found = true
				if !strings.Contains(step.Run, "release.py verify candidate") || !strings.Contains(step.Run, "tar -xzf candidate/payload.tar.gz") || step.Env["OMNIGREX_PI_DEPLOY_FILES"] == "" || strings.Contains(step.Run, ":sha-") {
					t.Fatal("smoke must use extracted payload and captured digest")
				}
			}
		}
		if !found {
			t.Fatal("missing smoke")
		}
	}
}
