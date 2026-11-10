package workflows

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ciJob struct {
	If    string    `yaml:"if"`
	Needs yaml.Node `yaml:"needs"`
	Steps []ciStep  `yaml:"steps"`
}

type ciStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
}

func readCIJobs(t *testing.T) map[string]ciJob {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate CI workflow test")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]ciJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow.Jobs
}

func ciNeeds(t *testing.T, job ciJob) []string {
	t.Helper()
	switch job.Needs.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		return []string{job.Needs.Value}
	case yaml.SequenceNode:
		var needs []string
		if err := job.Needs.Decode(&needs); err != nil {
			t.Fatal(err)
		}
		return needs
	default:
		t.Fatal("invalid CI needs")
		return nil
	}
}

func TestCIQualificationDependencies(t *testing.T) {
	jobs := readCIJobs(t)
	qualifications := []string{"docker-check", "development-qualification", "microsandbox"}
	for _, name := range qualifications {
		job, ok := jobs[name]
		if !ok {
			t.Fatalf("missing qualification %s", name)
		}
		if needs := ciNeeds(t, job); len(needs) != 0 || job.If != "" {
			t.Errorf("%s must run independently on every CI event, got needs %v, if %q", name, needs, job.If)
		}
	}
	if got := ciNeeds(t, jobs["check"]); !reflect.DeepEqual(got, qualifications) {
		t.Errorf("stable required check must gate all qualifications, got %v", got)
	}
	if got := jobs["check"].If; got != "always()" {
		t.Errorf("aggregate must run even when a qualification fails or is skipped, got %q", got)
	}
	if got := ciNeeds(t, jobs["publish"]); !reflect.DeepEqual(got, []string{"check"}) {
		t.Errorf("publication must require the aggregate, got %v", got)
	}
	if got := jobs["publish"].If; got != "github.event_name == 'push' && github.ref == 'refs/heads/main'" {
		t.Errorf("publication must remain main-push-only, got %q", got)
	}
}

func TestCIAggregateFailsClosed(t *testing.T) {
	job := readCIJobs(t)["check"]
	if len(job.Steps) != 1 || job.Steps[0].Run == "" {
		t.Fatal("expected one executable aggregate step")
	}
	step := job.Steps[0]
	// Execute the actual shell against every Actions prerequisite outcome.
	// Resolve the workflow's env expressions so a wrong needs reference fails too.
	results := []string{"success", "failure", "cancelled", "skipped", ""}
	for _, docker := range results {
		for _, development := range results {
			for _, microsandbox := range results {
				t.Run(fmt.Sprintf("docker=%s/development=%s/microsandbox=%s", docker, development, microsandbox), func(t *testing.T) {
					values := map[string]string{
						"${{ needs.docker-check.result }}":              docker,
						"${{ needs.development-qualification.result }}": development,
						"${{ needs.microsandbox.result }}":              microsandbox,
					}
					cmd := exec.Command("sh", "-ec", step.Run)
					cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
					for name, expression := range step.Env {
						value, ok := values[expression]
						if !ok {
							t.Fatalf("unexpected aggregate expression %q", expression)
						}
						cmd.Env = append(cmd.Env, name+"="+value)
					}
					output, err := cmd.CombinedOutput()
					wantSuccess := docker == "success" && development == "success" && microsandbox == "success"
					if (err == nil) != wantSuccess {
						t.Fatalf("success = %t, want %t: %s (%v)", err == nil, wantSuccess, output, err)
					}
				})
			}
		}
	}
}

func TestCITimingCaptureAndPublicationStayOnTheDockerRunner(t *testing.T) {
	jobs := readCIJobs(t)
	wantSteps := []string{"Build test timing tool", "Run unit tests", "Run Docker-backed integration tests", "Summarize test timings", "Upload test timings"}
	var found []string
	for jobName, job := range jobs {
		for _, step := range job.Steps {
			for _, name := range wantSteps {
				if step.Name != name {
					continue
				}
				if jobName != "docker-check" {
					t.Errorf("%s must run alongside its captures in docker-check, not %s", name, jobName)
					continue
				}
				found = append(found, name)
				switch name {
				case "Build test timing tool":
					if !strings.Contains(step.Run, `go build -o "$RUNNER_TEMP/omnigrex-test-timings" ./scripts/test-timings`) {
						t.Error("missing timing executable build")
					}
				case "Run unit tests":
					if !strings.Contains(step.Run, `run "$RUNNER_TEMP/test-timings" unit -- go test -race -count=1 -json ./...`) {
						t.Error("unit timing capture lost")
					}
				case "Run Docker-backed integration tests":
					for _, label := range []string{"integration", "pi-protocol"} {
						if !strings.Contains(step.Run, `run "$RUNNER_TEMP/test-timings" `+label+` -- go test`) {
							t.Errorf("%s timing capture lost", label)
						}
					}
				case "Summarize test timings":
					if step.If != "always()" || !strings.Contains(step.Run, `report "$RUNNER_TEMP/test-timings" unit integration pi-protocol >> "$GITHUB_STEP_SUMMARY"`) {
						t.Error("timing summary must retain all captures even after failure")
					}
				case "Upload test timings":
					if step.If != "always()" || !strings.HasPrefix(step.Uses, "actions/upload-artifact@") || step.With["name"] != "check-test-timings" || step.With["path"] != "${{ runner.temp }}/test-timings/" || step.With["retention-days"] != "7" {
						t.Error("timing artifacts must retain the capture directory even after failure")
					}
				}
			}
		}
	}
	if !reflect.DeepEqual(found, wantSteps) {
		t.Errorf("Docker timing steps = %v, want %v in that order", found, wantSteps)
	}
}

func TestCIDevelopmentQualificationUsesSelectedGoldenAndPropagatesFailures(t *testing.T) {
	job := readCIJobs(t)["development-qualification"]
	var commands []string
	for _, step := range job.Steps {
		if step.Run != "" {
			commands = append(commands, step.Run)
		}
	}
	if len(commands) == 0 {
		t.Fatal("missing development qualification commands")
	}
	for _, fail := range []string{"", "golden", "development", "module-source", "unit-suite"} {
		t.Run("fail="+fail, func(t *testing.T) {
			dir := t.TempDir()
			selection, err := os.ReadFile("../../agent/pi/selected-bundle.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "agent/pi"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent/pi/selected-bundle.json"), selection, 0600); err != nil {
				t.Fatal(err)
			}
			// An empty runner must pull the selected immutable baseline, not build checkout Pi.
			stub := `#!/bin/sh
set -eu
case "$*" in
  'pull ghcr.io/jozala/omnigrex/pi@sha256:09a7f498dd0593d7bee9410d82e641498796930c85200c93ad491faf323696a5')
    stage=golden ;;
  'build --platform linux/amd64 --file agent/environments/omnigrex/Dockerfile --target development --build-arg OMNIGREX_GOLDEN_IMAGE=ghcr.io/jozala/omnigrex/pi@sha256:09a7f498dd0593d7bee9410d82e641498796930c85200c93ad491faf323696a5 --tag omnigrex/development:dev .')
    test -f golden
    stage=development ;;
  *) echo "unexpected Docker command: $*" >&2; exit 2 ;;
esac
echo "$stage" >> events
test "$FAIL" != "$stage"
touch "$stage"
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o700); err != nil {
				t.Fatal(err)
			}
			for script, stage := range map[string]string{"test-development-image-cache.sh": "module-source", "test-development-image-unit.sh": "unit-suite"} {
				body := "set -eu\ntest -f development\necho " + stage + " >> events\ntest \"$FAIL\" != " + stage + "\n"
				if err := os.WriteFile(filepath.Join(dir, "scripts", script), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = nil
			for _, run := range commands {
				cmd := exec.Command("sh", "-ec", run)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "FAIL=" + fail}
				if output, runErr := cmd.CombinedOutput(); runErr != nil {
					t.Logf("qualification stopped: %s (%v)", output, runErr)
					err = runErr
					break
				}
			}
			if (err == nil) != (fail == "") {
				t.Fatalf("failure %q: result %v", fail, err)
			}
			data, readErr := os.ReadFile(filepath.Join(dir, "events"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := []string{"golden", "development", "module-source", "unit-suite"}
			for index, stage := range want {
				if stage == fail {
					want = want[:index+1]
					break
				}
			}
			if got := strings.Fields(string(data)); !reflect.DeepEqual(got, want) {
				t.Fatalf("qualification events = %v, want %v", got, want)
			}
		})
	}
}

func TestPreAdoptionQualificationCannotPublishOrOverrideProduction(t *testing.T) {
	jobs := readCIJobs(t)
	for _, backend := range []string{"docker", "microsandbox"} {
		j := jobs["qualify-pi-bundle-"+backend]
		if j.If != "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'" || len(ciNeeds(t, j)) != 0 {
			t.Fatal("qualification must be explicit, main-source-only and independent of publication")
		}
		tested, receipt := false, false
		for _, step := range j.Steps {
			if strings.Contains(step.Run, "${{") || strings.Contains(step.Run, "publish-pi-images") {
				t.Fatal("qualification executes dispatch shell or publishes images")
			}
			if strings.Contains(step.Run, "qualify-pi-bundle.sh") {
				tested = true
				if step.Env["OMNIGREX_PI_BACKEND"] != backend || step.Env["PI_CANDIDATE"] != "${{ inputs.pi_candidate }}" {
					t.Fatal("candidate not passed as data")
				}
			}
			if strings.Contains(step.Uses, "upload-artifact") {
				receipt = true
				if step.If != "" || step.With["name"] != "pi-bundle-"+backend+"-${{ github.run_id }}-${{ github.run_attempt }}" {
					t.Fatal("receipt must follow success and bind exact run/attempt/backend")
				}
			}
		}
		if !tested || !receipt {
			t.Fatal("missing qualification/evidence")
		}
	}
}

func TestPublicationCapturesCandidateOriginWithoutChangingProductSelection(t *testing.T) {
	job := readCIJobs(t)["publish"]
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "candidate-record") {
			if step.Env["OMNIGREX_PI_CANDIDATE_IMAGE"] != "${{ steps.images.outputs.pi_candidate_image }}" || step.Env["OMNIGREX_PI_CANDIDATE_SOURCE_COMMIT"] != "${{ github.sha }}" {
				t.Fatal("candidate must capture its own actual build origin, not selected-image provenance")
			}
			return
		}
	}
	t.Fatal("publication must retain an unqualified exact candidate record")
}

func TestRequiredGatesDoNotRequireCrossImageSessionMigration(t *testing.T) {
	found := false
	for _, step := range readCIJobs(t)["docker-check"].Steps {
		for _, line := range strings.Split(step.Run, "\n") {
			if strings.Contains(line, "sh scripts/test-pi-upgrade.sh") {
				found = true
				if !strings.Contains(line, "OMNIGREX_PI_UPGRADE_MODE=bundle") {
					t.Fatal("required Docker gate must not default to optional Session migration")
				}
			}
		}
	}
	if !found {
		t.Fatal("missing retained/fresh Docker qualification")
	}
	mise, err := os.ReadFile("../../mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"test-pi-upgrade", "test-pi-microsandbox-upgrade"} {
		if !strings.Contains(string(mise), "OMNIGREX_PI_UPGRADE_MODE=bundle mise run "+task+"\n") {
			t.Fatalf("required aggregate must qualify independent Sessions: %s", task)
		}
	}
}
