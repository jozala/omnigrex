package profile_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const testImage = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestNewOpenCodeV1ReturnsQualifiedContract(t *testing.T) {
	got, err := profile.NewOpenCodeV1(testImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}

	want := profile.Contract{
		Name:    "opencode-acp",
		Version: "v1",
		Image:   testImage,
		Platform: profile.Platform{
			OS:   "linux",
			Arch: "arm64",
		},
		User:    profile.User{UID: 10001, GID: 10001},
		Command: []string{"acp"},
		Environment: []profile.EnvironmentVariable{
			{Name: "HOME", Value: "/home/opencode"},
			{Name: "MISE_DATA_DIR", Value: "/home/opencode/.local/share/mise"},
			{Name: "OPENCODE_AUTH_CONTENT", Value: "{}"},
			{Name: "OPENCODE_AUTO_SHARE", Value: "false"},
			{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "1"},
			{Name: "OPENCODE_DISABLE_MODELS_FETCH", Value: "true"},
			{Name: "OPENCODE_DISABLE_SHARE", Value: "1"},
			{Name: "TMPDIR", Value: "/tmp/opencode"},
			{Name: "XDG_CACHE_HOME", Value: "/home/opencode/.cache"},
			{Name: "XDG_CONFIG_HOME", Value: "/home/opencode/.config"},
			{Name: "XDG_DATA_HOME", Value: "/home/opencode/.local/share"},
			{Name: "XDG_STATE_HOME", Value: "/home/opencode/.local/state"},
		},
		Mounts: []profile.Mount{
			{Name: "mise", Path: "/home/opencode/.local/share/mise"},
			{Name: "state", Path: "/home/opencode/.local/share/opencode"},
			{Name: "workspace", Path: "/workspace"},
		},
		Capabilities: []string{"session/list", "session/load", "session/resume"},
		Tmpfs: []profile.Tmpfs{
			{Path: "/home/opencode/.cache", SizeBytes: 64 << 20},
			{Path: "/home/opencode/.config", SizeBytes: 16 << 20},
			{Path: "/home/opencode/.local/state", SizeBytes: 16 << 20},
			{Path: "/home/opencode/.opencode", SizeBytes: 16 << 20},
			{Path: "/tmp/opencode", SizeBytes: 64 << 20, Executable: true},
		},
		Workspace:   "/workspace",
		MemoryBytes: 512 << 20,
		PIDsLimit:   128,
	}
	if contract := got.Contract(); !reflect.DeepEqual(contract, want) {
		t.Fatalf("Contract() = %#v, want %#v", contract, want)
	}
	if got.ContentSHA256() == "" {
		t.Fatal("ContentSHA256() is empty")
	}
}

func TestOpenCodeV1EnvironmentContainsOnlyRoleNeutralStaticValues(t *testing.T) {
	contract := validContract(t)
	reviewerOnly := map[string]struct{}{
		"OPENCODE_DISABLE_CLAUDE_CODE":     {},
		"OPENCODE_DISABLE_DEFAULT_PLUGINS": {},
		"OPENCODE_DISABLE_EXTERNAL_SKILLS": {},
		"OPENCODE_DISABLE_PROJECT_CONFIG":  {},
		"OPENCODE_PURE":                    {},
	}
	for _, variable := range contract.Environment {
		if variable.Name == "OPENCODE_CONFIG_CONTENT" {
			t.Fatal("static Runtime Profile contains turn-specific OpenCode configuration")
		}
		if _, found := reviewerOnly[variable.Name]; found {
			t.Fatalf("static Runtime Profile contains Reviewer-only environment key %q", variable.Name)
		}
	}
}

func TestNewRejectsContractsOutsideOpenCodeV1(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*profile.Contract)
	}{
		{name: "unknown profile", mutate: func(contract *profile.Contract) { contract.Name = "other" }},
		{name: "unknown version", mutate: func(contract *profile.Contract) { contract.Version = "v2" }},
		{name: "mutable image tag", mutate: func(contract *profile.Contract) { contract.Image = "registry.example/omnigrex/opencode:1.18.19" }},
		{name: "bare local image ID", mutate: func(contract *profile.Contract) {
			contract.Image = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{name: "short image digest", mutate: func(contract *profile.Contract) { contract.Image = "registry.example/opencode@sha256:aaaa" }},
		{name: "uppercase image digest", mutate: func(contract *profile.Contract) {
			contract.Image = "registry.example/opencode@sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		}},
		{name: "unsupported OS", mutate: func(contract *profile.Contract) { contract.Platform.OS = "darwin" }},
		{name: "unsupported architecture", mutate: func(contract *profile.Contract) { contract.Platform.Arch = "s390x" }},
		{name: "root UID", mutate: func(contract *profile.Contract) { contract.User.UID = 0 }},
		{name: "root GID", mutate: func(contract *profile.Contract) { contract.User.GID = 0 }},
		{name: "wrong command", mutate: func(contract *profile.Contract) { contract.Command = []string{"opencode", "acp"} }},
		{name: "missing environment", mutate: func(contract *profile.Contract) { contract.Environment = contract.Environment[1:] }},
		{name: "unknown environment", mutate: func(contract *profile.Contract) {
			contract.Environment = append(contract.Environment, profile.EnvironmentVariable{Name: "UNDECLARED", Value: "true"})
		}},
		{name: "duplicate environment", mutate: func(contract *profile.Contract) {
			contract.Environment = append(contract.Environment, contract.Environment[0])
		}},
		{name: "changed environment", mutate: func(contract *profile.Contract) { contract.Environment[0].Value = "/root" }},
		{name: "missing mount", mutate: func(contract *profile.Contract) { contract.Mounts = contract.Mounts[1:] }},
		{name: "unknown logical mount", mutate: func(contract *profile.Contract) {
			contract.Mounts = append(contract.Mounts, profile.Mount{Name: "credentials", Path: "/credentials"})
		}},
		{name: "duplicate logical mount", mutate: func(contract *profile.Contract) {
			contract.Mounts = append(contract.Mounts, contract.Mounts[0])
		}},
		{name: "overlapping mount", mutate: func(contract *profile.Contract) { contract.Mounts[1].Path = "/workspace/state" }},
		{name: "missing capability", mutate: func(contract *profile.Contract) { contract.Capabilities = contract.Capabilities[1:] }},
		{name: "unknown capability", mutate: func(contract *profile.Contract) {
			contract.Capabilities = append(contract.Capabilities, "terminal/create")
		}},
		{name: "duplicate capability", mutate: func(contract *profile.Contract) {
			contract.Capabilities = append(contract.Capabilities, contract.Capabilities[0])
		}},
		{name: "missing tmpfs", mutate: func(contract *profile.Contract) { contract.Tmpfs = contract.Tmpfs[1:] }},
		{name: "unknown tmpfs", mutate: func(contract *profile.Contract) {
			contract.Tmpfs = append(contract.Tmpfs, profile.Tmpfs{Path: "/other", SizeBytes: 1})
		}},
		{name: "duplicate tmpfs", mutate: func(contract *profile.Contract) {
			contract.Tmpfs = append(contract.Tmpfs, contract.Tmpfs[0])
		}},
		{name: "wrong tmpfs size", mutate: func(contract *profile.Contract) { contract.Tmpfs[0].SizeBytes++ }},
		{name: "executable home tmpfs", mutate: func(contract *profile.Contract) { contract.Tmpfs[0].Executable = true }},
		{name: "non-executable temp tmpfs", mutate: func(contract *profile.Contract) { contract.Tmpfs[4].Executable = false }},
		{name: "wrong workspace", mutate: func(contract *profile.Contract) { contract.Workspace = "/work" }},
		{name: "zero memory", mutate: func(contract *profile.Contract) { contract.MemoryBytes = 0 }},
		{name: "wrong memory", mutate: func(contract *profile.Contract) { contract.MemoryBytes++ }},
		{name: "zero PID limit", mutate: func(contract *profile.Contract) { contract.PIDsLimit = 0 }},
		{name: "wrong PID limit", mutate: func(contract *profile.Contract) { contract.PIDsLimit++ }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := validContract(t)
			test.mutate(&contract)
			if _, err := profile.New(contract); !errors.Is(err, profile.ErrInvalid) {
				t.Fatalf("New() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNewAcceptsSupportedPlatformsAndCanonicalizesOrder(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			contract := validContract(t)
			contract.Platform.Arch = arch
			slices.Reverse(contract.Environment)
			slices.Reverse(contract.Mounts)
			slices.Reverse(contract.Capabilities)
			slices.Reverse(contract.Tmpfs)

			got, err := profile.New(contract)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			want := validContract(t)
			want.Platform.Arch = arch
			if !reflect.DeepEqual(got.Contract(), want) {
				t.Fatalf("Contract() = %#v, want canonical %#v", got.Contract(), want)
			}
		})
	}
}

func TestProfileOwnsContractDataAndCanonicalHash(t *testing.T) {
	contract := validContract(t)
	slices.Reverse(contract.Environment)
	slices.Reverse(contract.Mounts)
	slices.Reverse(contract.Capabilities)
	slices.Reverse(contract.Tmpfs)
	before := cloneContract(contract)

	value, err := profile.New(contract)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !reflect.DeepEqual(contract, before) {
		t.Fatal("New() mutated its input contract")
	}
	baseline, err := profile.New(validContract(t))
	if err != nil {
		t.Fatalf("New() baseline error = %v", err)
	}
	if value.ContentSHA256() != baseline.ContentSHA256() {
		t.Fatalf("equivalent contracts have hashes %q and %q", value.ContentSHA256(), baseline.ContentSHA256())
	}
	if len(value.ContentSHA256()) != 64 {
		t.Fatalf("ContentSHA256() = %q, want 64 hex digits", value.ContentSHA256())
	}

	snapshot := value.Contract()
	snapshot.Command[0] = "changed"
	snapshot.Environment[0].Value = "changed"
	snapshot.Mounts[0].Path = "/changed"
	snapshot.Capabilities[0] = "changed"
	snapshot.Tmpfs[0].SizeBytes = 1
	if reflect.DeepEqual(snapshot, value.Contract()) {
		t.Fatal("Contract() returned profile-owned slices")
	}
	if value.ContentSHA256() != baseline.ContentSHA256() {
		t.Fatal("mutating a Contract() snapshot changed the profile hash")
	}

	changed := validContract(t)
	changed.Image = "registry.example/omnigrex/opencode@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	changedProfile, err := profile.New(changed)
	if err != nil {
		t.Fatalf("New() changed profile error = %v", err)
	}
	if changedProfile.ContentSHA256() == value.ContentSHA256() {
		t.Fatal("different contracts have the same canonical content hash")
	}
}

func TestRegistryResolvesExactImmutableReference(t *testing.T) {
	value, err := profile.New(validContract(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registry, err := profile.NewRegistry(value, value)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	resolved, err := registry.Resolve("opencode-acp", "v1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	snapshot := resolved.Contract()
	snapshot.Environment[0].Value = "changed"
	again, err := registry.Resolve("opencode-acp", "v1")
	if err != nil {
		t.Fatalf("Resolve() again error = %v", err)
	}
	if !reflect.DeepEqual(again.Contract(), value.Contract()) {
		t.Fatal("mutating a resolved contract changed the Registry")
	}

	for _, reference := range [][2]string{{"opencode-acp", ""}, {"opencode-acp", "v2"}, {"other", "v1"}} {
		if _, err := registry.Resolve(reference[0], reference[1]); !errors.Is(err, profile.ErrNotFound) {
			t.Errorf("Resolve(%q, %q) error = %v, want ErrNotFound", reference[0], reference[1], err)
		}
	}
}

func TestRegistryRejectsDuplicateReferenceConflict(t *testing.T) {
	first, err := profile.New(validContract(t))
	if err != nil {
		t.Fatalf("New() first error = %v", err)
	}
	contract := validContract(t)
	contract.Image = "registry.example/omnigrex/opencode@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	second, err := profile.New(contract)
	if err != nil {
		t.Fatalf("New() second error = %v", err)
	}

	if _, err := profile.NewRegistry(first, second); !errors.Is(err, profile.ErrConflict) {
		t.Fatalf("NewRegistry() error = %v, want ErrConflict", err)
	}
	if _, err := profile.NewRegistry(profile.Profile{}); !errors.Is(err, profile.ErrInvalid) {
		t.Fatalf("NewRegistry() zero profile error = %v, want ErrInvalid", err)
	}
}

func validContract(t *testing.T) profile.Contract {
	t.Helper()
	value, err := profile.NewOpenCodeV1(testImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}
	return value.Contract()
}

func cloneContract(contract profile.Contract) profile.Contract {
	contract.Command = slices.Clone(contract.Command)
	contract.Environment = slices.Clone(contract.Environment)
	contract.Mounts = slices.Clone(contract.Mounts)
	contract.Capabilities = slices.Clone(contract.Capabilities)
	contract.Tmpfs = slices.Clone(contract.Tmpfs)
	return contract
}
