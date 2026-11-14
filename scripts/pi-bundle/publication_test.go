package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
)

func TestPublicationModes(t *testing.T) {
	for _, mode := range []string{"product", "candidate"} {
		for _, fail := range []string{"", "build", "push", "digest"} {
			t.Run(mode+"/"+fail, func(t *testing.T) { testPublication(t, mode, fail) })
		}
	}
}

func testPublication(t *testing.T, mode, fail string) {
	bundle, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../publish-pi-images.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("publish.sh", string(script))
	summaryScript, err := os.ReadFile("../write-publication-summary.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	write("scripts/write-publication-summary.sh", string(summaryScript))
	validation, err := os.ReadFile("../validate-pi-dispatch.sh")
	if err != nil {
		t.Fatal(err)
	}
	write("scripts/validate-pi-dispatch.sh", string(validation))
	write("go", "#!/bin/sh\nset -eu\ntest \"$*\" = 'run ./scripts/pi-bundle image'\nprintf '%s\\n' \"$SELECTED\"\n")
	const candidateDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	write("docker", `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$EVENTS"
case "$1" in
 build|pull|push) test "$1" != "$FAIL"; exit 0 ;;
 image)
  test "$2" = inspect
  case "$5" in
   *Config.User*) echo 10001:10001 ;;
   *Architecture*) echo linux/amd64 ;;
   *Config.Labels*) echo https://github.com/jozala/omnigrex ;;
   *RepoDigests*) test "$FAIL" != digest; printf '%s@sha256:`+candidateDigest+`\n' "${3%:sha-*}" ;;
   *) exit 2 ;;
  esac ;;
 *) exit 2 ;;
esac
`)
	command := exec.Command("/bin/sh", "publish.sh", mode)
	command.Dir = dir
	command.Env = append(os.Environ(), "FAIL="+fail, "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_REF=refs/heads/main", "PI_MODE=build-candidate", "PI_CANDIDATE=", "PATH="+dir+":"+os.Getenv("PATH"), "SELECTED="+bundle.Image, "EVENTS="+filepath.Join(dir, "events"), "OMNIGREX_RELEASE_COMMIT="+strings.Repeat("a", 40), "GITHUB_OUTPUT="+filepath.Join(dir, "outputs"), "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
	if output, err := command.CombinedOutput(); fail != "" {
		if err == nil {
			t.Fatalf("publication ignored %s failure: %s", fail, output)
		}
		summary, _ := os.ReadFile(filepath.Join(dir, "summary"))
		if len(summary) != 0 {
			t.Fatal("failed publication emitted summary")
		}
		return
	} else if err != nil {
		t.Fatalf("publication: %v\n%s", err, output)
	}
	outputs, err := os.ReadFile(filepath.Join(dir, "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "candidate" {
		if string(outputs) != "pi_candidate_image=ghcr.io/jozala/omnigrex/pi@sha256:"+candidateDigest+"\n" {
			t.Fatalf("candidate outputs: %s", outputs)
		}
	} else {
		for _, want := range []string{"pi_image=" + bundle.Image + "\n", "orchestrator_image=ghcr.io/jozala/omnigrex/orchestrator@sha256:" + candidateDigest + "\n", "orchestrator_microsandbox_image=ghcr.io/jozala/omnigrex/orchestrator-microsandbox@sha256:" + candidateDigest + "\n", "development_image=ghcr.io/jozala/omnigrex/development@sha256:" + candidateDigest + "\n"} {
			if !strings.Contains(string(outputs), want) {
				t.Fatalf("missing %q: %s", want, outputs)
			}
		}
		if strings.Contains(string(outputs), "pi_candidate") {
			t.Fatalf("product emitted candidate: %s", outputs)
		}
	}
	summary, err := os.ReadFile(filepath.Join(dir, "summary"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "product" {
		if !strings.Contains(string(summary), "Selected production Pi: `"+bundle.Image+"`") || strings.Contains(string(summary), "Newly published Pi") {
			t.Fatalf("product summary: %s", summary)
		}
	} else if !strings.Contains(string(summary), "not qualification or adoption") {
		t.Fatalf("candidate summary: %s", summary)
	}

	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "candidate" {
		if strings.Contains(string(events), "orchestrator") || strings.Contains(string(events), "development") || strings.Contains(string(events), bundle.Image) {
			t.Fatalf("candidate published product: %s", events)
		}
	} else {
		if strings.Contains(string(events), "--file agent/pi/Dockerfile") || strings.Contains(string(events), "push ghcr.io/jozala/omnigrex/pi:") {
			t.Fatalf("ordinary publication built/pushed Pi: %s", events)
		}
		if !strings.Contains(string(events), "--build-arg OMNIGREX_GOLDEN_IMAGE="+bundle.Image) {
			t.Fatal("development omitted selected base")
		}
	}
}

func TestIndependentBundleQualificationCannotEmitMigrationEvidence(t *testing.T) {
	// Reject before invoking Docker or creating fixture resources, rather than
	// labelling retained/fresh checks as proof of cross-image compatibility.
	command := exec.Command("/bin/sh", "../test-pi-upgrade.sh")
	command.Env = append(os.Environ(), "OMNIGREX_PI_UPGRADE_MODE=bundle", "OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE="+filepath.Join(t.TempDir(), "migration.json"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "does not emit Session migration compatibility results") {
		t.Fatalf("migration evidence accepted for independent bundle qualification: %v: %s", err, output)
	}
}

func TestCandidateCaptureUsesPublishedDigestAndCompleteSourceInventory(t *testing.T) {
	image := "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("f", 64)
	source := strings.Repeat("a", 40)
	command := exec.Command("go", "run", "./scripts/pi-bundle", "candidate-record")
	command.Dir = "../.."
	command.Env = append(os.Environ(), "OMNIGREX_PI_CANDIDATE_IMAGE="+image, "OMNIGREX_PI_CANDIDATE_SOURCE_COMMIT="+source)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("capture: %v: %s", err, output)
	}
	record, err := piresources.ParseCandidate(output)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := piresources.NormalizeContract(piresources.CandidateContract("amd64"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Image != image || record.SourceCommit != source || record.Platform != "linux/amd64" || string(record.Resources) != string(inventory) {
		t.Fatal("captured identity must contain actual pushed digest, original checkout commit/platform and complete checkout inventory")
	}
	for _, invalidImage := range []string{"", "ghcr.io/jozala/omnigrex/pi:sha-" + source} {
		command := exec.Command("go", "run", "./scripts/pi-bundle", "candidate-record")
		command.Dir = "../.."
		command.Env = append(os.Environ(), "OMNIGREX_PI_CANDIDATE_IMAGE="+invalidImage, "OMNIGREX_PI_CANDIDATE_SOURCE_COMMIT="+source)
		if err := command.Run(); err == nil {
			t.Fatalf("capture accepted unpublished or mutable identity %q", invalidImage)
		}
	}
}
