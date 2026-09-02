//go:build integration

package deployment

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"
)

const (
	postgresImage = "postgres:18-alpine@sha256:b40d931bd0e7ce6eecc59a5a6ac3b3c04a01e559750e73e7086b6dbd7f8bf545"
	agentImage    = "omnigrex/opencode:1.18.19"
	dockerSocket  = "/var/run/docker.sock"
)

var composeSecretNames = []string{
	"omnigrex-database-password",
	"omnigrex-github-developer-private-key",
	"omnigrex-github-reviewer-private-key",
	"omnigrex-github-webhook-secret",
	"omnigrex-developer-provider-credentials",
	"omnigrex-reviewer-provider-credentials",
}

type composeConfig struct {
	Services map[string]composeService  `json:"services"`
	Networks map[string]composeNetwork  `json:"networks"`
	Volumes  map[string]composeResource `json:"volumes"`
	Secrets  map[string]json.RawMessage `json:"secrets"`
}

type composeService struct {
	Image       string                       `json:"image"`
	User        string                       `json:"user"`
	ReadOnly    bool                         `json:"read_only"`
	CapDrop     []string                     `json:"cap_drop"`
	SecurityOpt []string                     `json:"security_opt"`
	Init        bool                         `json:"init"`
	MemoryLimit string                       `json:"mem_limit"`
	PIDsLimit   int64                        `json:"pids_limit"`
	GroupAdd    []string                     `json:"group_add"`
	NetworkMode string                       `json:"network_mode"`
	Networks    map[string]json.RawMessage   `json:"networks"`
	Ports       []composePort                `json:"ports"`
	Volumes     []composeMount               `json:"volumes"`
	DependsOn   map[string]composeDependency `json:"depends_on"`
	Secrets     []composeSecret              `json:"secrets"`
	Environment map[string]string            `json:"environment"`
}

type composePort struct {
	Target    int    `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
}

type composeMount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type composeDependency struct {
	Condition string `json:"condition"`
}

type composeSecret struct {
	Source string `json:"source"`
}

type composeNetwork struct {
	Name     string `json:"name"`
	Internal bool   `json:"internal"`
}

type composeResource struct {
	Name string `json:"name"`
}

func TestComposeTopology(t *testing.T) {
	config := loadComposeConfig(t)
	assertExactKeys(t, "services", config.Services, "postgres", "opencode-image", "orchestrator")
	assertExactKeys(t, "secrets", config.Secrets, composeSecretNames...)

	postgres := config.Services["postgres"]
	validator := config.Services["opencode-image"]
	orchestrator := config.Services["orchestrator"]

	assertServiceSandbox(t, "postgres", postgres, "70:70")
	assertServiceSandbox(t, "opencode-image", validator, "10001:10001")
	assertServiceSandbox(t, "orchestrator", orchestrator, "10001:10001")

	if postgres.Image != postgresImage {
		t.Errorf("postgres image = %q, want pinned %q", postgres.Image, postgresImage)
	}
	if validator.Image != agentImage {
		t.Errorf("opencode-image image = %q, want pinned %q", validator.Image, agentImage)
	}
	if got := orchestrator.Environment["OMNIGREX_AGENT_IMAGE_REFERENCE"]; got != agentImage {
		t.Errorf("orchestrator agent image reference = %q, want %q", got, agentImage)
	}

	assertExactKeys(t, "postgres networks", postgres.Networks, "backend")
	if len(postgres.Ports) != 0 {
		t.Errorf("postgres published ports = %+v, want none", postgres.Ports)
	}
	assertMounts(t, "postgres", postgres.Volumes, composeMount{
		Type: "volume", Source: "postgres-data", Target: "/var/lib/postgresql",
	})
	assertSecretSources(t, "postgres", postgres.Secrets, "omnigrex-database-password")
	if !slices.Contains(postgres.GroupAdd, "456") {
		t.Errorf("postgres supplementary groups = %v, want secret-file group 456", postgres.GroupAdd)
	}

	if validator.NetworkMode != "none" {
		t.Errorf("opencode-image network mode = %q, want none", validator.NetworkMode)
	}
	if len(validator.Networks) != 0 {
		t.Errorf("opencode-image networks = %v, want none", sortedKeys(validator.Networks))
	}
	if len(validator.Ports) != 0 {
		t.Errorf("opencode-image published ports = %+v, want none", validator.Ports)
	}
	if len(validator.Secrets) != 0 {
		t.Errorf("opencode-image secrets = %+v, want none", validator.Secrets)
	}
	assertMounts(t, "opencode-image", validator.Volumes,
		composeMount{Type: "volume", Source: "workspaces", Target: "/workspace"},
		composeMount{Type: "volume", Source: "runtime-state", Target: "/home/opencode/.local/share/opencode"},
		composeMount{Type: "volume", Source: "mise-data", Target: "/home/opencode/.local/share/mise"},
	)

	assertExactKeys(t, "orchestrator networks", orchestrator.Networks, "agent", "backend")
	if len(orchestrator.Ports) != 1 {
		t.Errorf("orchestrator published ports = %+v, want only host 8080 to app 8080/tcp", orchestrator.Ports)
	} else {
		port := orchestrator.Ports[0]
		if port.Target != 8080 || port.Published != "8080" || port.Protocol != "tcp" {
			t.Errorf("orchestrator published port = %+v, want host 8080 to app 8080/tcp", port)
		}
	}
	assertMounts(t, "orchestrator", orchestrator.Volumes, composeMount{
		Type: "bind", Source: dockerSocket, Target: dockerSocket, ReadOnly: true,
	})
	assertSecretSources(t, "orchestrator", orchestrator.Secrets, composeSecretNames...)
	if !slices.Contains(orchestrator.GroupAdd, "123") || !slices.Contains(orchestrator.GroupAdd, "456") {
		t.Errorf("orchestrator supplementary groups = %v, want Docker group 123 and secret-file group 456", orchestrator.GroupAdd)
	}
	assertExactKeys(t, "orchestrator dependencies", orchestrator.DependsOn, "opencode-image", "postgres")
	if got := orchestrator.DependsOn["postgres"].Condition; got != "service_healthy" {
		t.Errorf("orchestrator postgres dependency condition = %q, want service_healthy", got)
	}
	if got := orchestrator.DependsOn["opencode-image"].Condition; got != "service_completed_successfully" {
		t.Errorf("orchestrator opencode-image dependency condition = %q, want service_completed_successfully", got)
	}

	assertSoleDockerSocketMount(t, config.Services, "orchestrator")
	assertStableNetworks(t, config.Networks)
	assertStableVolumes(t, config.Volumes)
}

func loadComposeConfig(t *testing.T) composeConfig {
	t.Helper()
	output, err := executeCompose(
		repositoryRoot(t),
		composeEnvironment("/dev/null", "123", "456", "8080", nil),
		30*time.Second,
		"config", "--format", "json",
	)
	if err != nil {
		t.Fatalf("render compose.yaml: %v\n%s", err, output)
	}

	var config composeConfig
	if err := json.Unmarshal(output, &config); err != nil {
		t.Fatalf("decode Docker Compose JSON: %v\n%s", err, output)
	}
	return config
}

func assertServiceSandbox(t *testing.T, name string, service composeService, user string) {
	t.Helper()
	if service.User != user {
		t.Errorf("%s user = %q, want %q", name, service.User, user)
	}
	if !service.ReadOnly {
		t.Errorf("%s root filesystem is writable", name)
	}
	if !slices.Equal(service.CapDrop, []string{"ALL"}) {
		t.Errorf("%s cap_drop = %v, want [ALL]", name, service.CapDrop)
	}
	if !slices.Contains(service.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("%s security_opt = %v, want no-new-privileges:true", name, service.SecurityOpt)
	}
	if !service.Init {
		t.Errorf("%s init = false, want true", name)
	}
	memory, err := strconv.ParseInt(service.MemoryLimit, 10, 64)
	if err != nil || memory <= 0 {
		t.Errorf("%s mem_limit = %q, want a finite positive byte limit", name, service.MemoryLimit)
	}
	if service.PIDsLimit <= 0 {
		t.Errorf("%s pids_limit = %d, want a finite positive limit", name, service.PIDsLimit)
	}
}

func assertMounts(t *testing.T, service string, got []composeMount, want ...composeMount) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s mounts = %+v, want %+v", service, got, want)
		return
	}
	byTarget := make(map[string]composeMount, len(got))
	for _, mount := range got {
		byTarget[mount.Target] = mount
	}
	for _, expected := range want {
		if actual, ok := byTarget[expected.Target]; !ok || actual != expected {
			t.Errorf("%s mount at %q = %+v, want %+v", service, expected.Target, actual, expected)
		}
	}
}

func assertSecretSources(t *testing.T, service string, got []composeSecret, want ...string) {
	t.Helper()
	sources := make([]string, 0, len(got))
	for _, secret := range got {
		sources = append(sources, secret.Source)
	}
	slices.Sort(sources)
	expected := slices.Clone(want)
	slices.Sort(expected)
	if !slices.Equal(sources, expected) {
		t.Errorf("%s secrets = %v, want %v", service, sources, expected)
	}
}

func assertSoleDockerSocketMount(t *testing.T, services map[string]composeService, wantService string) {
	t.Helper()
	var owners []string
	for serviceName, service := range services {
		for _, mount := range service.Volumes {
			if mount.Source == dockerSocket || mount.Target == dockerSocket {
				owners = append(owners, serviceName)
			}
		}
	}
	if !slices.Equal(owners, []string{wantService}) {
		t.Errorf("Docker socket mount owners = %v, want only %s", owners, wantService)
	}
}

func assertStableNetworks(t *testing.T, networks map[string]composeNetwork) {
	t.Helper()
	assertExactKeys(t, "networks", networks, "agent", "backend")
	if got := networks["backend"]; got.Name != "omnigrex-backend" || !got.Internal {
		t.Errorf("backend network = %+v, want stable name omnigrex-backend and internal=true", got)
	}
	if got := networks["agent"]; got.Name != "omnigrex-agent" || got.Internal {
		t.Errorf("agent network = %+v, want stable name omnigrex-agent and internal=false", got)
	}
}

func assertStableVolumes(t *testing.T, volumes map[string]composeResource) {
	t.Helper()
	want := map[string]string{
		"postgres-data": "omnigrex-postgres",
		"workspaces":    "omnigrex-workspaces",
		"runtime-state": "omnigrex-runtime-state",
		"mise-data":     "omnigrex-mise",
	}
	assertExactKeys(t, "volumes", volumes, "mise-data", "postgres-data", "runtime-state", "workspaces")
	for key, name := range want {
		if got := volumes[key].Name; got != name {
			t.Errorf("volume %s name = %q, want %q", key, got, name)
		}
	}
}

func assertExactKeys[V any](t *testing.T, label string, got map[string]V, want ...string) {
	t.Helper()
	actual := sortedKeys(got)
	expected := slices.Clone(want)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("%s = %v, want exactly %v", label, actual, expected)
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
