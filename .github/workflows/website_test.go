package workflows

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type websiteJob struct {
	ciJob       `yaml:",inline"`
	Name        string            `yaml:"name"`
	Env         map[string]string `yaml:"env"`
	Permissions map[string]string `yaml:"permissions"`
	Outputs     map[string]string `yaml:"outputs"`
	Environment struct {
		Name string `yaml:"name"`
	} `yaml:"environment"`
	Concurrency struct {
		Group  string `yaml:"group"`
		Cancel bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
}

type websiteWorkflow struct {
	On          map[string]yaml.Node  `yaml:"on"`
	Permissions map[string]string     `yaml:"permissions"`
	Env         map[string]string     `yaml:"env"`
	Jobs        map[string]websiteJob `yaml:"jobs"`
}

func websiteRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate website workflow tests")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
}

func readWebsiteWorkflow(t *testing.T) websiteWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(websiteRoot(t), ".github/workflows/website.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow websiteWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestWebsiteWorkflowBoundaries(t *testing.T) {
	workflow := readWebsiteWorkflow(t)
	if _, ok := workflow.On["pull_request"]; !ok {
		t.Fatal("required website check must run on Pull Requests")
	}
	if workflow.On["pull_request"].Kind != yaml.ScalarNode {
		t.Error("do not path-filter the required Pull Request workflow")
	}
	if _, ok := workflow.On["pull_request_target"]; ok {
		t.Error("website builds must not use pull_request_target")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Error("missing manual redeployment")
	}
	var push struct {
		Branches []string `yaml:"branches"`
	}
	pushNode := workflow.On["push"]
	if err := pushNode.Decode(&push); err != nil || !reflect.DeepEqual(push.Branches, []string{"main"}) {
		t.Fatalf("push must target main only: %v, %v", push.Branches, err)
	}
	if !reflect.DeepEqual(workflow.Permissions, map[string]string{"contents": "read"}) || len(workflow.Env) != 0 {
		t.Error("workflow must be read-only and keep credentials out of global environment")
	}
	for name, job := range workflow.Jobs {
		if len(job.Permissions) != 0 {
			t.Errorf("%s unexpectedly overrides read-only token permissions", name)
		}
		if name != "deploy" && (len(job.Env) != 0 || job.Environment.Name != "") {
			t.Errorf("%s must not acquire deployment environment credentials", name)
		}
		for _, step := range job.Steps {
			if step.Uses != "" && !regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`).MatchString(step.Uses) {
				t.Errorf("action is not commit-pinned: %s", step.Uses)
			}
			for _, value := range step.Env {
				if name != "deploy" && strings.Contains(value, "secrets.") {
					t.Errorf("%s exposes deployment credentials", name)
				}
			}
		}
	}
	deploy := workflow.Jobs["deploy"]
	if deploy.If != "(github.event_name == 'push' || github.event_name == 'workflow_dispatch') && github.ref == 'refs/heads/main' && needs.changes.outputs.changed == 'true'" {
		t.Errorf("deployment must remain main-only and exclude PRs: %q", deploy.If)
	}
	if got := ciNeeds(t, deploy.ciJob); !reflect.DeepEqual(got, []string{"changes", "build", "website-check"}) {
		t.Errorf("deployment must depend on qualified output: %v", got)
	}
	if deploy.Environment.Name != "website-production" || deploy.Concurrency.Group != "website-production" || deploy.Concurrency.Cancel {
		t.Error("deployment must use the isolated environment and serialize uploads without cancellation")
	}
	build := workflow.Jobs["build"]
	if build.If != "needs.changes.outputs.changed == 'true'" {
		t.Errorf("expensive build must be conditional: %q", build.If)
	}
	var uploaded, downloaded string
	for _, step := range build.Steps {
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			uploaded = step.With["name"]
			if step.ID != "artifact" {
				t.Error("build must expose the ID returned by its artifact upload")
			}
			if step.With["path"] != "website/dist/" || step.With["if-no-files-found"] != "error" || step.With["include-hidden-files"] != "true" {
				t.Error("artifact must contain only the complete generated static output")
			}
		}
	}
	for _, step := range deploy.Steps {
		if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
			downloaded = step.With["artifact-ids"]
			if step.With["merge-multiple"] != "true" || step.With["path"] != "website-dist" {
				t.Error("download must put the artifact contents directly in the upload source directory")
			}
		}
	}
	if !strings.Contains(uploaded, "github.run_attempt") || build.Outputs["artifact_id"] != "${{ steps.artifact.outputs.artifact-id }}" || downloaded != "${{ needs.build.outputs.artifact_id }}" {
		t.Error("deployment must use the successful build's artifact ID, including when only a failed deployment is rerun")
	}
}

func TestWebsiteRequiredCheckReportsAndFailsClosed(t *testing.T) {
	job := readWebsiteWorkflow(t).Jobs["website-check"]
	if job.Name != "Website check" || job.If != "always()" || !reflect.DeepEqual(ciNeeds(t, job.ciJob), []string{"changes", "build"}) || len(job.Steps) != 1 {
		t.Fatal("required check must always report after change detection and build")
	}
	step := job.Steps[0]
	for _, changes := range []string{"success", "failure", "cancelled", "skipped", ""} {
		for _, changed := range []string{"true", "false", ""} {
			for _, build := range []string{"success", "failure", "cancelled", "skipped", ""} {
				t.Run(fmt.Sprintf("%s/%s/%s", changes, changed, build), func(t *testing.T) {
					values := map[string]string{"${{ needs.changes.result }}": changes, "${{ needs.changes.outputs.changed }}": changed, "${{ needs.build.result }}": build}
					cmd := exec.Command("sh", "-ec", step.Run)
					cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
					for name, expression := range step.Env {
						value, ok := values[expression]
						if !ok {
							t.Fatalf("unknown gate input %q", expression)
						}
						cmd.Env = append(cmd.Env, name+"="+value)
					}
					output, err := cmd.CombinedOutput()
					want := changes == "success" && (changed == "true" && build == "success" || changed == "false" && build == "skipped")
					if (err == nil) != want {
						t.Fatalf("success=%t, want %t: %s", err == nil, want, output)
					}
				})
			}
		}
	}
}

func TestWebsiteBuildOnlyExportsAfterQualification(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(websiteRoot(t), "website/scripts/build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []string{"", "build", "run", "cp"} {
		t.Run("fail="+fail, func(t *testing.T) {
			dir := t.TempDir()
			scripts := filepath.Join(dir, "website/scripts")
			if err := os.MkdirAll(scripts, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scripts, "build.sh"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$WEBSITE_TEST_EVENTS"
test "$1" != "$WEBSITE_TEST_FAIL"
case "$1" in
  run) case "$*" in *'/repository:ro'*'npm run check') ;; *) exit 5 ;; esac ;;
  cp) test "$2" = 'website-build-fixture-1:/website/dist/.'; printf 'static output' > "$3/index.html" ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", filepath.Join(scripts, "build.sh"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "WEBSITE_TEST_EVENTS="+filepath.Join(dir, "events"), "WEBSITE_TEST_FAIL="+fail, "GITHUB_RUN_ID=fixture", "GITHUB_RUN_ATTEMPT=1")
			output, err := cmd.CombinedOutput()
			if (err == nil) != (fail == "") {
				t.Fatalf("build failure propagation: %s, %v", output, err)
			}
			var want []string
			for _, stage := range []string{"build", "run", "cp"} {
				want = append(want, stage)
				if stage == fail {
					break
				}
			}
			want = append(want, "rm")
			events, err := os.ReadFile(filepath.Join(dir, "events"))
			if err != nil || !reflect.DeepEqual(strings.Fields(string(events)), want) {
				t.Fatalf("build events=%q, want %v: %v", events, want, err)
			}
		})
	}
}

func TestWebsiteDeployIsOneDirectRsyncAndCleansCredentials(t *testing.T) {
	for _, scenario := range []struct {
		name, port, fail, missingKey string
	}{
		{name: "default port"},
		{name: "upload failure", port: "2222", fail: "23"},
		{name: "missing configuration", missingKey: "true"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			stub := "#!/bin/sh\nexec \"$WEBSITE_TEST_BINARY\" -test.run='^TestWebsiteRsyncHelper$' -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			key := "fixture-private-key"
			if scenario.missingKey != "" {
				key = ""
			}
			cmd := exec.Command("sh", filepath.Join(websiteRoot(t), "website/scripts/deploy.sh"), "static-output/")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "RUNNER_TEMP="+dir, "WEBSITE_SSH_HOST=example.invalid", "WEBSITE_SSH_USER=operator", "WEBSITE_DEPLOY_PATH=/srv/website root/", "WEBSITE_SSH_PORT="+scenario.port, "WEBSITE_SSH_KEY="+key, "WEBSITE_SSH_KNOWN_HOSTS=fixture-known-host", "WEBSITE_TEST_BINARY="+binary, "WEBSITE_TEST_ARGS="+filepath.Join(dir, "arguments"), "WEBSITE_TEST_FAIL="+scenario.fail, "WEBSITE_RSYNC_HELPER=1")
			output, err := cmd.CombinedOutput()
			wantSuccess := scenario.fail == "" && scenario.missingKey == ""
			if (err == nil) != wantSuccess {
				t.Fatalf("upload failure propagation: %s, %v", output, err)
			}
			left, err := filepath.Glob(filepath.Join(dir, "website-ssh.*"))
			if err != nil || len(left) != 0 {
				t.Fatalf("credentials were not removed: %v, %v", left, err)
			}
			data, readErr := os.ReadFile(filepath.Join(dir, "arguments"))
			if scenario.missingKey != "" {
				if !os.IsNotExist(readErr) {
					t.Fatal("rsync must not run without configuration")
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			if len(args) != 10 || args[0] != "-az" || args[1] != "--delete" || args[2] != "--protect-args" || args[5] != "-e" || args[7] != "--" || args[8] != "static-output/" || args[9] != "operator@example.invalid:/srv/website root/" {
				t.Fatalf("unexpected upload arguments: %q", args)
			}
			port := scenario.port
			if port == "" {
				port = "22"
			}
			for _, option := range []string{"StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "BatchMode=yes", "UserKnownHostsFile=", "-p \"" + port + "\""} {
				if !strings.Contains(args[6], option) {
					t.Errorf("SSH transport lost %q", option)
				}
			}
		})
	}
}

// A subprocess stand-in for rsync inspects credentials while they exist, without
// starting SSH or contacting any remote system.
func TestWebsiteRsyncHelper(t *testing.T) {
	if os.Getenv("WEBSITE_RSYNC_HELPER") != "1" {
		return
	}
	dirs, err := filepath.Glob(filepath.Join(os.Getenv("RUNNER_TEMP"), "website-ssh.*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("missing isolated credential directory: %v, %v", dirs, err)
	}
	for file, want := range map[string]string{"id_ed25519": "fixture-private-key\n", "known_hosts": "fixture-known-host\n"} {
		filename := filepath.Join(dirs[0], file)
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("credential file must be mode 0600: %s, %v", filename, err)
		}
		data, err := os.ReadFile(filename)
		if err != nil || string(data) != want {
			t.Fatalf("credential contents were not preserved: %s, %v", filename, err)
		}
	}
	for index, arg := range os.Args {
		if arg == "--" {
			if err := os.WriteFile(os.Getenv("WEBSITE_TEST_ARGS"), []byte(strings.Join(os.Args[index+1:], "\x00")+"\x00"), 0o600); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("WEBSITE_TEST_FAIL") == "23" {
				os.Exit(23)
			}
			os.Exit(0)
		}
	}
	t.Fatal("missing rsync arguments")
}
