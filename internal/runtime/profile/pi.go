package profile

import (
	"bytes"
	"encoding/json"
	piresources "github.com/jozala/omnigrex/agent/pi"
)

func NewPiV1(image string, platform Platform) (Profile, error) {
	return New(piContract(image, platform))
}

func NewBuiltinV1(name, image string, platform Platform) (Profile, error) {
	switch name {
	case "pi-rpc":
		return NewPiV1(image, platform)
	default:
		return Profile{}, ErrNotFound
	}
}

func piContract(image string, platform Platform) Contract {
	return Contract{Name: "pi-rpc", Version: "v1", Image: image, Platform: platform, User: User{UID: 10001, GID: 10001}, Command: []string{"/usr/local/bin/node", "/opt/omnigrex/pi/runner.mjs"}, Workspace: "/workspace", PIDsLimit: 128,
		Environment:  []EnvironmentVariable{{Name: "HOME", Value: "/tmp"}, {Name: "PATH", Value: "/usr/local/bin:/usr/bin:/bin"}, {Name: "PI_CODING_AGENT_DIR", Value: "/run/pi-agent"}, {Name: "PI_OFFLINE", Value: "1"}, {Name: "PI_TELEMETRY", Value: "0"}, {Name: "PI_SKIP_VERSION_CHECK", Value: "1"}, {Name: "TMPDIR", Value: "/tmp"}},
		Mounts:       []Mount{{Name: "workspace", Path: "/workspace"}, {Name: "state", Path: "/state"}, {Name: "mise", Path: "/home/opencode/.local/share/mise"}, {Name: "tool-data", Path: "/home/opencode/.local/share/omnigrex-tool-data"}},
		Tmpfs:        []Tmpfs{{Path: "/tmp", SizeBytes: 64 << 20, Executable: true}, {Path: "/run/pi-agent", SizeBytes: 16 << 20}},
		Capabilities: []string{"exact-session", "history-replay", "checked-configuration", "authenticated-mcp"}, RepositoryEnvironment: &RepositoryEnvironment{Version: 1}, GoldenResourceContract: piresources.CurrentContract(platform.Arch)}
}

func validatePi(contract Contract) error {
	if !IsExactRegistryImage(contract.Image) || !IsSupportedPlatform(contract.Platform) || contract.RepositoryEnvironment == nil {
		return invalid("Pi requires an exact supported repository environment")
	}
	expected := piContract(contract.Image, contract.Platform)
	expected.RepositoryEnvironment = contract.RepositoryEnvironment
	if len(contract.GoldenResourceContract) > 0 {
		if _, err := piresources.NormalizeContract(contract.GoldenResourceContract); err != nil {
			return invalid("Pi golden resource contract is invalid")
		}
	}
	expected.GoldenResourceContract = contract.GoldenResourceContract
	actual, _ := json.Marshal(normalizeContract(contract))
	want, _ := json.Marshal(normalizeContract(expected))
	if !bytes.Equal(actual, want) {
		return invalid("Pi runtime resources, paths or process contract mismatch")
	}
	return nil
}
