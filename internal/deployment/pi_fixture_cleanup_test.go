package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func assertNamedFixtureContainerCleanup(t *testing.T, calls, prefix, suffix string) {
	t.Helper()
	var created, removed string
	for _, line := range strings.Split(calls, "\n") {
		// Some script stubs also record the selected project and file set.
		if fields := strings.SplitN(line, "|", 3); len(fields) == 3 {
			line = fields[2]
		}
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		if words[0] == "run" {
			for i, word := range words {
				if word == "--name" && i+1 < len(words) {
					name := words[i+1]
					if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
						created = name
					}
				}
			}
		}
		if words[0] == "rm" {
			if len(words) != 4 || words[1] != "-f" || words[2] != "-v" {
				t.Fatalf("container cleanup must include anonymous volumes and one owned name: %s", line)
			}
			removed = words[3]
		}
		if strings.Contains(line, "prune") {
			t.Fatalf("fixture cleanup pruned shared resources: %s", line)
		}
	}
	if created == "" || removed != created {
		t.Fatalf("cleanup did not target the named fixture container: created=%q removed=%q\n%s", created, removed, calls)
	}
}

func TestPiWorkflowCleanupRemovesOnlyOwnedContainerVolumes(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			stub := `#!/bin/sh
printf '%s\n' "$*" >> "${0%/*}/calls"
case "$1 $2" in
  'image inspect')
    case "$*" in *Architecture*) printf 'amd64\n';; *) printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';; esac;;
  'run --rm')
    case "$*" in
      *'/usr/bin/stat'*) printf '0\n';;
      *'omnigrex/pi-workflow-qualification:step12'*) exit 42;;
    esac;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-workflow.sh")
			command.Dir = deployTestRepositoryRoot(t)
			command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
				"OMNIGREX_PI_BACKEND="+backend, "OMNIGREX_MICROSANDBOX_KVM_GID=1")
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("failed qualification reported success: %s", output)
			}
			content, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			calls := string(content)
			assertNamedFixtureContainerCleanup(t, calls, "pi-workflow-", "-database")
			created := make(map[string]bool)
			removed := make(map[string]bool)
			for _, line := range strings.Split(calls, "\n") {
				words := strings.Fields(line)
				if (strings.HasPrefix(line, "volume create ") || strings.HasPrefix(line, "network create ")) && len(words) == 3 {
					created[words[0]+":"+words[2]] = true
				}
				if strings.HasPrefix(line, "volume rm ") || strings.HasPrefix(line, "network rm ") {
					for _, name := range words[2:] {
						resource := words[0] + ":" + name
						if !created[resource] {
							t.Fatalf("removed foreign resource: %s", line)
						}
						removed[resource] = true
					}
				}
			}
			if len(created) != 5 || len(removed) != len(created) {
				t.Fatalf("fixture cleanup missed created resources: created=%v removed=%v", created, removed)
			}
		})
	}
}

func TestPiNativeProvisioningFailureCleansAuthorizedFileSet(t *testing.T) {
	root := t.TempDir()
	stub := `#!/bin/sh
printf '%s|%s|%s\n' "$COMPOSE_PROJECT_NAME" "$COMPOSE_FILE" "$*" >> "${0%/*}/calls"
case "$1 $2" in
  'image inspect') printf 'amd64\n';;
  'compose run') exit 42;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
	command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-deployment.sh")
	command.Dir = deployTestRepositoryRoot(t)
	command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
		"OMNIGREX_PI_BACKEND=microsandbox", "OMNIGREX_PI_CUTOVER_REHEARSAL=0", "OMNIGREX_MICROSANDBOX_KVM_GID=1",
		"OMNIGREX_PI_DEPLOY_IMAGE="+image, "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE="+image)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("failed native provisioning reported success: %s", output)
	}
	content, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "|compose.pi.yaml:compose.pi-microsandbox.yaml:smoke.yaml:native-smoke.yaml|compose down --volumes --remove-orphans --timeout 10") {
		t.Fatalf("native provisioning failure lost authorized fixture cleanup: %s", content)
	}
}
