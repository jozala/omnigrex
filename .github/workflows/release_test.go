package workflows

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
		if len(job.Steps) != 3 || job.Steps[2].Run != "go run ./scripts/release "+channel+" release-assets" {
			t.Fatal("release must only designate existing artifacts")
		}
		if job.Steps[2].Env["GH_TOKEN"] != "${{ github.token }}" || job.Steps[2].Env["OMNIGREX_RELEASE_APP_ID"] != "${{ vars.OMNIGREX_RELEASE_APP_ID }}" || job.Steps[2].Env["OMNIGREX_RELEASE_APP_PRIVATE_KEY"] != "${{ secrets.OMNIGREX_RELEASE_APP_PRIVATE_KEY }}" {
			t.Fatal("release credentials must remain separated and explicit")
		}
		if !strings.HasPrefix(job.Steps[1].Uses, "actions/setup-go@") || job.Steps[1].With["go-version-file"] != "go.mod" {
			t.Fatal("release must use the pinned Go toolchain")
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
				if !strings.Contains(step.Run, "go run ./scripts/release verify candidate") || !strings.Contains(step.Run, "tar -xzf candidate/payload.tar.gz") || step.Env["OMNIGREX_PI_DEPLOY_FILES"] == "" || strings.Contains(step.Run, ":sha-") {
					t.Fatal("smoke must use extracted payload and captured digest")
				}
			}
		}
		if !found {
			t.Fatal("missing smoke")
		}
	}
}

func TestAppCreatedTagsCannotTriggerReleaseOrCIBuildLoops(t *testing.T) {
	b, err := os.ReadFile("ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On map[string]yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatal(err)
	}
	var push struct {
		Branches []string `yaml:"branches"`
		Tags     []string `yaml:"tags"`
	}
	node := workflow.On["push"]
	if err := node.Decode(&push); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(push.Branches, []string{"main"}) || len(push.Tags) != 0 {
		t.Fatal("App-created release tags must not cause new CI builds")
	}
	// The manual-only event check above covers both release entry points.
}
