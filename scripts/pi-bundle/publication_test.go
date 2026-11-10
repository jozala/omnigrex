package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
)

func TestPublicationDoesNotAdoptTheRebuiltPiCandidate(t *testing.T) {
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
	write("go", "#!/bin/sh\nset -eu\ntest \"$*\" = 'run ./scripts/pi-bundle image'\nprintf '%s\\n' \"$SELECTED\"\n")
	const candidateDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	write("docker", `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$EVENTS"
case "$1" in
 build|pull|push) exit 0 ;;
 image)
  test "$2" = inspect
  case "$5" in
   *Config.User*) echo 10001:10001 ;;
   *Architecture*) echo linux/amd64 ;;
   *Config.Labels*) echo https://github.com/jozala/omnigrex ;;
   *RepoDigests*) printf '%s@sha256:`+candidateDigest+`\n' "${3%:sha-*}" ;;
   *) exit 2 ;;
  esac ;;
 *) exit 2 ;;
esac
`)
	command := exec.Command("/bin/sh", "publish.sh")
	command.Dir = dir
	command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "SELECTED="+bundle.Image, "EVENTS="+filepath.Join(dir, "events"), "OMNIGREX_RELEASE_COMMIT="+strings.Repeat("a", 40), "GITHUB_OUTPUT="+filepath.Join(dir, "outputs"), "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("publication: %v\n%s", err, output)
	}
	outputs, err := os.ReadFile(filepath.Join(dir, "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(outputs), "pi_image="+bundle.Image+"\n") || !strings.Contains(string(outputs), "pi_candidate_image=ghcr.io/jozala/omnigrex/pi@sha256:"+candidateDigest+"\n") {
		t.Fatalf("candidate conflated with selected Pi: %s", outputs)
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "--build-arg OMNIGREX_GOLDEN_IMAGE="+bundle.Image) || strings.Contains(string(events), "--build-arg OMNIGREX_GOLDEN_IMAGE=ghcr.io/jozala/omnigrex/pi@sha256:"+candidateDigest) {
		t.Fatal("derived publication silently adopted candidate")
	}
}
