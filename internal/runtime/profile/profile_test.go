package profile_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

const testImage = "registry.example/omnigrex/pi@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestIsExactRegistryImageUsesCanonicalDistributionGrammar(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, image := range []string{
		"registry.example:5000/namespace/image@sha256:" + digest,
		"[2001:db8::1]:5000/namespace/image@sha256:" + digest,
		"registry.example/namespace/image__name@sha256:" + digest,
		"registry.example/namespace/image---name@sha256:" + digest,
	} {
		if !profile.IsExactRegistryImage(image) {
			t.Errorf("IsExactRegistryImage(%q) = false", image)
		}
	}
	for _, image := range []string{
		"namespace/image@sha256:" + digest,
		"registry.example/namespace/image:latest@sha256:" + digest,
		"registry.example/namespace/image@sha512:" + strings.Repeat("a", 128),
		"registry.example/namespace/image@sha256:" + strings.ToUpper(digest),
		"not an image@sha256:" + digest,
	} {
		if profile.IsExactRegistryImage(image) {
			t.Errorf("IsExactRegistryImage(%q) = true", image)
		}
	}
}

func TestNewPiV2ReturnsQualifiedContract(t *testing.T) {
	got, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewPiV2() error = %v", err)
	}

	want := profile.Contract{
		Name:    "pi-rpc",
		Version: "v2",
		Image:   testImage,
		Platform: profile.Platform{
			OS:   "linux",
			Arch: "arm64",
		},
		User:    profile.User{UID: 10001, GID: 10001},
		Command: []string{"/usr/local/bin/node", "/opt/omnigrex/pi/runner.mjs"},
		Environment: []profile.EnvironmentVariable{
			{Name: "HOME", Value: "/tmp"},
			{Name: "PATH", Value: "/usr/local/bin:/usr/bin:/bin"},
			{Name: "PI_CODING_AGENT_DIR", Value: "/run/pi-agent"},
			{Name: "PI_OFFLINE", Value: "1"},
			{Name: "PI_SKIP_VERSION_CHECK", Value: "1"},
			{Name: "PI_TELEMETRY", Value: "0"},
			{Name: "TMPDIR", Value: "/tmp"},
		},
		Mounts: []profile.Mount{
			{Name: "mise", Path: "/home/omnigrex/.local/share/mise"},
			{Name: "state", Path: "/state"},
			{Name: "tool-data", Path: "/home/omnigrex/.local/share/omnigrex-tool-data"},
			{Name: "workspace", Path: "/workspace"},
		},
		Capabilities: []string{"authenticated-mcp", "checked-configuration", "exact-session", "history-replay"},
		Tmpfs: []profile.Tmpfs{
			{Path: "/run/pi-agent", SizeBytes: 16 << 20},
			{Path: "/tmp", SizeBytes: 64 << 20, Executable: true},
		},
		Workspace:              "/workspace",
		PIDsLimit:              128,
		RepositoryEnvironment:  &profile.RepositoryEnvironment{Version: 1},
		GoldenResourceContract: piresources.CurrentContract("arm64"),
	}
	if contract := got.Contract(); !reflect.DeepEqual(contract, want) {
		t.Fatalf("Contract() = %#v, want %#v", contract, want)
	}
	if got.ContentSHA256() == "" {
		t.Fatal("ContentSHA256() is empty")
	}
}

func TestPiV1EnvironmentContainsOnlyRoleNeutralStaticValues(t *testing.T) {
	contract := validContract(t)
	for _, variable := range contract.Environment {
		if strings.Contains(variable.Name, "AUTH") || strings.Contains(variable.Name, "TOKEN") || strings.Contains(variable.Name, "MODEL") || strings.Contains(variable.Name, "THINKING") || strings.Contains(variable.Name, "PERMISSION") {
			t.Fatalf("static Runtime Profile contains Turn-specific configuration %q", variable.Name)
		}
	}
}

func TestNewRejectsContractsOutsidePiV1(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*profile.Contract)
	}{
		{name: "unknown profile", mutate: func(contract *profile.Contract) { contract.Name = "other" }},
		{name: "unsupported profile", mutate: func(contract *profile.Contract) { contract.Name = "unsupported-runtime" }},
		{name: "unknown version", mutate: func(contract *profile.Contract) { contract.Version = "v3" }},
		{name: "mutable image tag", mutate: func(contract *profile.Contract) { contract.Image = "registry.example/omnigrex/pi:latest" }},
		{name: "bare local image ID", mutate: func(contract *profile.Contract) {
			contract.Image = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{name: "short image digest", mutate: func(contract *profile.Contract) { contract.Image = "registry.example/pi@sha256:aaaa" }},
		{name: "uppercase image digest", mutate: func(contract *profile.Contract) {
			contract.Image = "registry.example/pi@sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		}},
		{name: "unsupported OS", mutate: func(contract *profile.Contract) { contract.Platform.OS = "darwin" }},
		{name: "unsupported architecture", mutate: func(contract *profile.Contract) { contract.Platform.Arch = "s390x" }},
		{name: "root UID", mutate: func(contract *profile.Contract) { contract.User.UID = 0 }},
		{name: "root GID", mutate: func(contract *profile.Contract) { contract.User.GID = 0 }},
		{name: "wrong command", mutate: func(contract *profile.Contract) { contract.Command = []string{"node", "runner.mjs"} }},
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
		{name: "executable configuration tmpfs", mutate: func(contract *profile.Contract) { contract.Tmpfs[0].Executable = true }},
		{name: "non-executable temp tmpfs", mutate: func(contract *profile.Contract) { contract.Tmpfs[1].Executable = false }},
		{name: "missing repository environment", mutate: func(contract *profile.Contract) { contract.RepositoryEnvironment = nil }},
		{name: "unsupported environment version", mutate: func(contract *profile.Contract) { contract.RepositoryEnvironment.Version = 2 }},
		{name: "unknown requirement", mutate: func(contract *profile.Contract) { contract.RepositoryEnvironment.Requirements = []string{"unknown"} }},
		{name: "duplicate requirement", mutate: func(contract *profile.Contract) {
			contract.RepositoryEnvironment.Requirements = []string{"tool-installation", "tool-installation"}
		}},
		{name: "wrong workspace", mutate: func(contract *profile.Contract) { contract.Workspace = "/work" }},
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
	snapshot.RepositoryEnvironment.Version = 2
	snapshot.GoldenResourceContract[0] = 'x'
	if reflect.DeepEqual(snapshot, value.Contract()) {
		t.Fatal("Contract() returned profile-owned slices")
	}
	if value.ContentSHA256() != baseline.ContentSHA256() {
		t.Fatal("mutating a Contract() snapshot changed the profile hash")
	}
	if !reflect.DeepEqual(value.Contract(), baseline.Contract()) {
		t.Fatal("mutating a Contract() snapshot changed profile-owned resources")
	}

	changed := validContract(t)
	changed.Image = "registry.example/omnigrex/pi@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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

	resolved, err := registry.Resolve("pi-rpc", "v2")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	snapshot := resolved.Contract()
	snapshot.Environment[0].Value = "changed"
	again, err := registry.Resolve("pi-rpc", "v2")
	if err != nil {
		t.Fatalf("Resolve() again error = %v", err)
	}
	if !reflect.DeepEqual(again.Contract(), value.Contract()) {
		t.Fatal("mutating a resolved contract changed the Registry")
	}

	for _, reference := range [][2]string{{"pi-rpc", ""}, {"pi-rpc", "v1"}, {"pi-rpc", "v3"}, {"other", "v1"}} {
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
	contract.Image = "registry.example/omnigrex/pi@sha256:" + strings.Repeat("b", 64)
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

func TestCatalogResolvesCurrentAndHistoricalImmutableBindings(t *testing.T) {
	oldContract := validContract(t)
	oldContract.Image = "registry.example/omnigrex/pi@sha256:" + strings.Repeat("1", 64)
	oldProfile, err := profile.New(oldContract)
	if err != nil {
		t.Fatal(err)
	}
	candidateContract := validContract(t)
	candidateContract.Image = "registry.example/omnigrex/pi@sha256:" + strings.Repeat("2", 64)
	candidateProfile, err := profile.New(candidateContract)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := profile.NewCatalog([]profile.Profile{candidateProfile}, []profile.Profile{oldProfile})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}

	current, err := catalog.Resolve("pi-rpc", "v2")
	if err != nil || current.ContentSHA256() != candidateProfile.ContentSHA256() {
		t.Fatalf("Resolve() current = (%s, %v), want candidate", current.ContentSHA256(), err)
	}
	historical, err := catalog.ResolveBinding(oldProfile.Binding())
	if err != nil || historical.ContentSHA256() != oldProfile.ContentSHA256() {
		t.Fatalf("ResolveBinding() historical = (%s, %v), want old", historical.ContentSHA256(), err)
	}
	conflicting := oldProfile.Binding()
	conflicting.Image = candidateProfile.Contract().Image
	if _, err := catalog.ResolveBinding(conflicting); !errors.Is(err, profile.ErrNotFound) {
		t.Fatalf("ResolveBinding() mixed binding error = %v, want ErrNotFound", err)
	}
	currentOnly, err := profile.NewRegistry(candidateProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := currentOnly.ResolveBinding(oldProfile.Binding()); !errors.Is(err, profile.ErrNotFound) {
		t.Fatalf("ResolveBinding() fabricated uncatalogued historical contract: %v", err)
	}
}

func validContract(t *testing.T) profile.Contract {
	t.Helper()
	value, err := profile.NewPiV2(testImage, profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewPiV2() error = %v", err)
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
