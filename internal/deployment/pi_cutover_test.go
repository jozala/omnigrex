package deployment_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPiCutoverStopsBeforeResetOnBackupFailure(t *testing.T) {
	calls := runFailedCutover(t, "backup")
	if !strings.Contains(calls, "pg_dump") {
		t.Fatal("rehearsal did not attempt a pre-reset backup")
	}
	// The failure trap may remove the disposable fixture, but the procedure
	// itself must not reset the installation after a failed recovery export.
	if strings.Contains(calls, "down --volumes --remove-orphans --timeout 60") {
		t.Fatal("cutover reset proceeded without a valid backup")
	}
	if !strings.Contains(calls, "|compose.legacy.yaml:smoke.yaml:legacy.yaml|compose down --volumes --remove-orphans --timeout 10") {
		t.Fatal("validated legacy fixture was not cleaned after backup failure")
	}
}

func TestPiCutoverRejectsSurvivingLegacyVolume(t *testing.T) {
	calls := runFailedCutover(t, "surviving-volume")
	if !strings.Contains(calls, "down --volumes --remove-orphans --timeout 60") || !strings.Contains(calls, "volume ls --filter name=pi-deploy-") {
		t.Fatal("rehearsal did not verify explicit legacy disposal")
	}
	if strings.Contains(calls, "compose.pi.yaml:compose.pi-docker.yaml:smoke.yaml|compose up") {
		t.Fatal("Pi started while legacy storage survived")
	}
}

func TestPiCutoverRejectsUnknownStopInventory(t *testing.T) {
	calls := runFailedCutover(t, "inventory")
	if !strings.Contains(calls, "ps --quiet") || strings.Contains(calls, "pg_dump") {
		t.Fatal("unavailable stop inventory was treated as empty before backup")
	}
}

func TestPiCutoverRejectsUnknownVolumeInventory(t *testing.T) {
	calls := runFailedCutover(t, "volume-inventory")
	if !strings.Contains(calls, "volume ls --filter name=pi-deploy-") {
		t.Fatal("rehearsal did not attempt post-reset storage inventory")
	}
	if strings.Contains(calls, "compose.pi.yaml:compose.pi-docker.yaml:smoke.yaml|compose up") {
		t.Fatal("Pi started without confirmed legacy volume absence")
	}
}

func TestPiCutoverImageSelectionsReachBackup(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		for _, targets := range []string{"published", "built"} {
			t.Run(backend+"/published-source/"+targets+"-target", func(t *testing.T) {
				overrides := []string{"OMNIGREX_PI_BACKEND=" + backend, "OMNIGREX_MICROSANDBOX_KVM_GID=1"}
				if targets == "built" {
					overrides = append(overrides, "OMNIGREX_PI_DEPLOY_IMAGE=", "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE=")
				}
				calls := runFailedCutover(t, "backup", overrides...)
				if !strings.Contains(calls, "pg_dump") {
					t.Fatal("image selection failed before reaching the backup gate")
				}
				if targets == "built" && backend == "docker" {
					if strings.Count(calls, "--network host") != 1 {
						t.Fatal("fixture publication requires exactly one initialized registry")
					}
					assertNamedFixtureContainerCleanup(t, calls, "pi-deploy-", "-registry")
				}
			})
		}
	}
}

func TestPiCutoverRequiresExternalPublishedSource(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides []string
	}{
		{"missing bundle", []string{"OMNIGREX_PI_CUTOVER_SOURCE_COMPOSE_FILE="}},
		{"unavailable bundle", []string{"OMNIGREX_PI_CUTOVER_SOURCE_COMPOSE_FILE=/nonexistent/old-release.yaml"}},
		{"missing source pair", []string{"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE=", "OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE="}},
		{"incomplete source pair", []string{"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE="}},
		{"mutable source image", []string{"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE=registry.example/agent:old"}},
		{"short digest", []string{"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE=registry.example/agent@sha256:abc"}},
		{"tagged digest", []string{"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE=registry.example/agent:old@sha256:" + strings.Repeat("a", 64)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if calls := runFailedCutover(t, "backup", test.overrides...); calls != "" {
				t.Fatalf("invalid source touched Docker: %s", calls)
			}
		})
	}
}

func TestPiCutoverRejectsUnisolatedHistoricalBundles(t *testing.T) {
	modes := []string{"config-failure", "source-build", "external-volume", "extra-volume", "foreign-bind"}
	for _, volume := range []string{"postgres-data", "mise-data", "runtime-state", "workspaces"} {
		for _, backing := range []string{"bind", "nfs", "custom-driver"} {
			modes = append(modes, "rendered-"+backing+":"+volume)
		}
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			calls := runFailedCutover(t, mode)
			if !strings.Contains(calls, "compose config --format json") || strings.Contains(calls, "|compose up") || strings.Contains(calls, "pg_dump") {
				t.Fatalf("unisolated source was not rejected before startup: %s", calls)
			}
			for _, line := range strings.Split(calls, "\n") {
				if strings.Contains(line, "|compose.legacy.yaml:smoke.yaml:legacy.yaml|compose down") {
					t.Fatalf("rejected historical bundle was used for destructive cleanup: %s", line)
				}
			}
		})
	}
}

func TestPiCutoverRenderedLocalVolumesReachStartup(t *testing.T) {
	calls := runFailedCutover(t, "rendered-default:postgres-data")
	if !strings.Contains(calls, "|compose up") {
		t.Fatalf("ordinary local fixture volumes failed isolation validation: %s", calls)
	}
}

func TestPiCutoverCleanupSwitchesToFreshPiFileSet(t *testing.T) {
	for _, backend := range []string{"docker", "microsandbox"} {
		t.Run(backend, func(t *testing.T) {
			calls := runFailedCutover(t, "pi-startup", "OMNIGREX_PI_BACKEND="+backend, "OMNIGREX_MICROSANDBOX_KVM_GID=1")
			if !strings.Contains(calls, "|compose.legacy.yaml:smoke.yaml:legacy.yaml|compose down --volumes --remove-orphans --timeout 60") {
				t.Fatal("legacy fixture reset did not complete")
			}
			selected := "compose.pi.yaml:compose.pi-" + backend + ".yaml:smoke.yaml"
			if backend == "microsandbox" {
				selected += ":native-smoke.yaml"
			}
			if !strings.Contains(calls, "|"+selected+"|compose down --volumes --remove-orphans --timeout 10") {
				t.Fatalf("fresh Pi failure did not clean its authorized file set: %s", calls)
			}
			if strings.Contains(calls, "|compose.legacy.yaml:smoke.yaml:legacy.yaml|compose down --volumes --remove-orphans --timeout 10") {
				t.Fatal("fresh Pi cleanup reused the disposed legacy file set")
			}
		})
	}
}

func runFailedCutover(t *testing.T, mode string, overrides ...string) string {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "old-release.yaml")
	content := []byte("# externally-selected-old-release\nservices: {}\n")
	var realDocker string
	var selectedVolume string
	var backing map[string]any
	if strings.HasPrefix(mode, "rendered-") {
		var err error
		realDocker, err = exec.LookPath("docker")
		if err != nil {
			t.Fatal(err)
		}
		kind, volume, _ := strings.Cut(strings.TrimPrefix(mode, "rendered-"), ":")
		selectedVolume = volume
		backing = map[string]any{"driver": "local", "driver_opts": map[string]string{"type": "none", "o": "bind", "device": "/srv/production/postgres"}}
		if kind == "default" {
			backing = map[string]any{"driver": "local"}
		} else if kind == "nfs" {
			backing["driver_opts"] = map[string]string{"type": "nfs", "o": "addr=192.0.2.10,rw", "device": ":/production/state"}
		} else if kind == "custom-driver" {
			backing = map[string]any{"driver": "production-storage-plugin"}
		}
		// The actual Compose merge retains backing options even though smoke.yaml
		// replaces each installation volume's name. Only config uses real Docker;
		// startup, pulls, and cleanup remain behind the controlled boundary.
		var document map[string]any
		if err := yaml.Unmarshal([]byte(`services:
  postgres:
    image: postgres:18
    volumes: [postgres-data:/var/lib/postgresql]
    networks: [backend]
  opencode-image:
    image: registry.example/agent:old
    volumes: [mise-data:/mise]
    networks: [agent]
  orchestrator:
    image: registry.example/orchestrator:old
    volumes: [runtime-state:/state, workspaces:/workspace]
    networks: [agent, backend]
    secrets: [provider-credentials]
networks:
  agent: {}
  backend: {}
volumes:
  postgres-data: {}
  mise-data: {}
  runtime-state: {}
  workspaces: {}
secrets:
  provider-credentials:
    file: ${OMNIGREX_PROVIDER_CREDENTIALS_FILE}
`), &document); err != nil {
			t.Fatal(err)
		}
		document["volumes"].(map[string]any)[volume] = backing
		encoded, err := yaml.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		content = append([]byte("# externally-selected-old-release\n"), encoded...)
	}
	if err := os.WriteFile(bundle, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "calls"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
printf '%s|%s|%s\n' "$COMPOSE_PROJECT_NAME" "$COMPOSE_FILE" "$*" >> "${0%/*}/calls"
case "$1 $2" in
  'image inspect')
    case "$*" in
      *RepoDigests*) printf 'registry.example/fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';;
      *) printf 'arm64\n';;
    esac;;
  'ps --quiet') if [ "$FAILURE_MODE" = inventory ]; then exit 42; fi;;
  'volume ls')
    if [ "$FAILURE_MODE" = volume-inventory ]; then exit 42; fi
    if [ "$FAILURE_MODE" = pi-startup ]; then exit 0; fi
    printf 'legacy-survivor\n';;
  'compose config')
    case "$FAILURE_MODE" in rendered-*)
      REAL_DOCKER "$@" > "${0%/*}/rendered.json" || exit $?
      cat "${0%/*}/rendered.json"
      exit 0;;
    esac
    if [ "$FAILURE_MODE" = config-failure ]; then exit 42; fi
    jq -n --arg namespace "$COMPOSE_PROJECT_NAME" --arg mode "$FAILURE_MODE" '
      {
        services: {postgres: {volumes: [{type: "volume", source: "postgres-data", target: "/var/lib/postgresql"}]}, "opencode-image": {image: "registry.example/pi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, orchestrator: {image: "registry.example/pi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", volumes: [{type: "volume", source: "runtime-state", target: "/state"}, {type: "bind", source: "/var/run/docker.sock", target: "/var/run/docker.sock"}, {type: "bind", source: "/dev/null", target: "/compatibility"}], environment: {OMNIGREX_OPENCODE_ACP_V1_IMAGE: "registry.example/pi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},
        networks: {agent: {name: ($namespace+"-agent")}, backend: {name: ($namespace+"-backend")}},
        volumes: {"postgres-data": {name: ($namespace+"-postgres")}, "mise-data": {name: ($namespace+"-mise")}, "runtime-state": {name: ($namespace+"-runtime-state")}, workspaces: {name: ($namespace+"-workspaces")}}, secrets: {}
      }
      | if $mode == "source-build" then .services.orchestrator.build = {context: "/production"}
        elif $mode == "external-volume" then .volumes.workspaces.external = true
        elif $mode == "extra-volume" then .volumes.production = {name: "production"}
        elif $mode == "foreign-bind" then .services.orchestrator.volumes = [{type: "bind", source: "/production", target: "/data"}]
        else . end';;
  'compose exec')
    case "$*" in
      *pg_dump*) if [ "$FAILURE_MODE" = backup ]; then exit 42; fi;;
    esac;;
  'compose up')
    case "$COMPOSE_FILE" in
      compose.legacy.yaml:*)
        if ! grep -q '^# externally-selected-old-release$' compose.legacy.yaml; then exit 43; fi
        case "$FAILURE_MODE" in rendered-*) exit 42;; esac;;
      compose.pi.yaml:*) if [ "$FAILURE_MODE" = pi-startup ]; then exit 42; fi;;
    esac;;
  'run --rm')
    case "$*" in
      *'tar -czf'*) tar -czf - -T /dev/null;;
    esac;;
esac
exit 0
`
	// compose() intentionally strips ambient configuration, so persist the
	// fault selection in the stub rather than relying on forwarded environment.
	stub = strings.ReplaceAll(stub, `"$FAILURE_MODE"`, `"`+mode+`"`)
	stub = strings.ReplaceAll(stub, "REAL_DOCKER", strconv.Quote(realDocker))
	if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/pi@sha256:" + strings.Repeat("a", 64)
	command := exec.CommandContext(t.Context(), "sh", "scripts/test-pi-deployment.sh")
	command.Dir = deployTestRepositoryRoot(t)
	command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
		"OMNIGREX_PI_BACKEND=docker", "OMNIGREX_PI_CUTOVER_REHEARSAL=1", "COMPOSE_PROJECT_NAME=production",
		"OMNIGREX_PI_DEPLOY_IMAGE="+image, "OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE="+image,
		"OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE="+image, "OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE="+image,
		"OMNIGREX_PI_CUTOVER_SOURCE_COMPOSE_FILE="+bundle)
	command.Env = append(command.Env, overrides...)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("incomplete cutover reported success: %s", output)
	}
	content, err = os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	calls := string(content)
	if realDocker != "" {
		rendered, err := os.ReadFile(filepath.Join(root, "rendered.json"))
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Volumes map[string]struct {
				Name       string
				Driver     string
				DriverOpts map[string]string `json:"driver_opts"`
			}
		}
		if err := json.Unmarshal(rendered, &document); err != nil {
			t.Fatal(err)
		}
		volume := document.Volumes[selectedVolume]
		if !strings.HasPrefix(volume.Name, "pi-deploy-") || volume.Driver != backing["driver"] {
			t.Fatalf("fixture did not preserve backing driver under generated name: %+v", volume)
		}
		if options, ok := backing["driver_opts"].(map[string]string); ok {
			for name, value := range options {
				if volume.DriverOpts[name] != value {
					t.Fatalf("Compose did not retain %s=%s: %+v", name, value, volume)
				}
			}
		}
	}
	if strings.Contains(calls, "cutover-source") || strings.Contains(calls, "agent/opencode/Dockerfile") {
		t.Fatal("rehearsal rebuilt removed source runtime")
	}
	for _, line := range strings.Split(calls, "\n") {
		if strings.Contains(line, "|compose ") && !strings.HasPrefix(line, "pi-deploy-") {
			t.Fatalf("rehearsal touched an ambient project: %s", line)
		}
	}
	return calls
}
