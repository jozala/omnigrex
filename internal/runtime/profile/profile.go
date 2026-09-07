package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var (
	ErrInvalid  = errors.New("invalid Runtime Profile")
	ErrConflict = errors.New("Runtime Profile reference conflict")
	ErrNotFound = errors.New("Runtime Profile not found")
)

var registryImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)

// Platform identifies the operating system and architecture for an image digest.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// User is the numeric identity used by the Runtime Process.
type User struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// EnvironmentVariable is one exact static environment entry.
type EnvironmentVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Mount names a persistent path without selecting a deployment volume or subpath.
type Mount struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Tmpfs describes one bounded writable temporary filesystem.
type Tmpfs struct {
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	Executable bool   `json:"executable"`
}

// Contract is the complete versioned Runtime Profile content.
type Contract struct {
	Name         string                `json:"name"`
	Version      string                `json:"version"`
	Image        string                `json:"image"`
	Platform     Platform              `json:"platform"`
	User         User                  `json:"user"`
	Command      []string              `json:"command"`
	Environment  []EnvironmentVariable `json:"environment"`
	Mounts       []Mount               `json:"mounts"`
	Capabilities []string              `json:"capabilities"`
	Tmpfs        []Tmpfs               `json:"tmpfs"`
	Workspace    string                `json:"workspace"`
	MemoryBytes  int64                 `json:"memory_bytes"`
	PIDsLimit    int64                 `json:"pids_limit"`
}

// Profile is an immutable validated Runtime Profile.
type Profile struct {
	contract      Contract
	contentSHA256 string
}

// Binding is the immutable Runtime Profile identity persisted by an Assignment and Agent Session.
// Platform is part of the content identified by ContentSHA256 and is recovered only by exact resolution.
type Binding struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	ContentSHA256 string `json:"content_sha256"`
	Image         string `json:"image_digest"`
}

// Catalog contains explicitly current Runtime Profiles and current or historical immutable profiles.
type Catalog struct {
	current  map[reference]Profile
	profiles map[bindingReference]Profile
}

// Registry is an immutable collection whose profiles are all current.
type Registry struct {
	Catalog
}

type reference struct {
	name    string
	version string
}

type bindingReference struct {
	reference
	contentSHA256 string
	image         string
}

// NewOpenCodeV1 constructs the qualified opencode-acp/v1 contract.
// Its tmpfs sizes are 64 MiB for cache, 16 MiB each for config, state, and .opencode,
// and 64 MiB for the executable /tmp/opencode.
func NewOpenCodeV1(image string, platform Platform) (Profile, error) {
	contract := Contract{
		Name:     "opencode-acp",
		Version:  "v1",
		Image:    image,
		Platform: platform,
		User:     User{UID: 10001, GID: 10001},
		Command:  []string{"acp"},
		Environment: []EnvironmentVariable{
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
		Mounts: []Mount{
			{Name: "mise", Path: "/home/opencode/.local/share/mise"},
			{Name: "state", Path: "/home/opencode/.local/share/opencode"},
			{Name: "workspace", Path: "/workspace"},
		},
		Capabilities: []string{"session/list", "session/load", "session/resume"},
		Tmpfs: []Tmpfs{
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
	return New(contract)
}

// New validates and snapshots an opencode-acp/v1 contract.
func New(contract Contract) (Profile, error) {
	if err := validate(contract); err != nil {
		return Profile{}, err
	}
	contract = cloneContract(contract)
	sort.Slice(contract.Environment, func(i, j int) bool { return contract.Environment[i].Name < contract.Environment[j].Name })
	sort.Slice(contract.Mounts, func(i, j int) bool { return contract.Mounts[i].Name < contract.Mounts[j].Name })
	slices.Sort(contract.Capabilities)
	sort.Slice(contract.Tmpfs, func(i, j int) bool { return contract.Tmpfs[i].Path < contract.Tmpfs[j].Path })
	return newProfile(contract)
}

func newProfile(contract Contract) (Profile, error) {
	contract = cloneContract(contract)
	canonical, err := json.Marshal(contract)
	if err != nil {
		return Profile{}, fmt.Errorf("encode Runtime Profile: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return Profile{contract: contract, contentSHA256: hex.EncodeToString(digest[:])}, nil
}

// Contract returns a defensive copy of the validated contract.
func (profile Profile) Contract() Contract {
	return cloneContract(profile.contract)
}

// ContentSHA256 returns the lowercase hexadecimal digest of canonical contract content.
func (profile Profile) ContentSHA256() string {
	return profile.contentSHA256
}

// NewCatalog snapshots current and historical profiles. Historical profiles may share a
// name and version with a current profile, but current references must be unambiguous.
func NewCatalog(current, historical []Profile) (Catalog, error) {
	catalog := Catalog{
		current:  make(map[reference]Profile, len(current)),
		profiles: make(map[bindingReference]Profile, len(current)+len(historical)),
	}
	for _, candidate := range append(slices.Clone(current), historical...) {
		if err := validate(candidate.contract); err != nil || candidate.contentSHA256 == "" {
			if err != nil {
				return Catalog{}, err
			}
			return Catalog{}, invalid("profile has no canonical content hash")
		}
		key := bindingReference{
			reference:     reference{name: candidate.contract.Name, version: candidate.contract.Version},
			contentSHA256: candidate.contentSHA256,
			image:         candidate.contract.Image,
		}
		catalog.profiles[key] = cloneProfile(candidate)
	}
	for _, candidate := range current {
		key := reference{name: candidate.contract.Name, version: candidate.contract.Version}
		if existing, ok := catalog.current[key]; ok && existing.contentSHA256 != candidate.contentSHA256 {
			return Catalog{}, fmt.Errorf("%w: current %s/%s", ErrConflict, key.name, key.version)
		}
		catalog.current[key] = cloneProfile(candidate)
	}
	return catalog, nil
}

// NewRegistry snapshots profiles and rejects conflicting current content for one reference.
func NewRegistry(profiles ...Profile) (Registry, error) {
	catalog, err := NewCatalog(profiles, nil)
	if err != nil {
		return Registry{}, err
	}
	return Registry{Catalog: catalog}, nil
}

// Resolve returns the explicitly configured current profile matching the name and version.
func (catalog Catalog) Resolve(name, version string) (Profile, error) {
	value, ok := catalog.current[reference{name: name, version: version}]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %s/%s", ErrNotFound, name, version)
	}
	return cloneProfile(value), nil
}

// ResolveBinding returns only a profile whose complete persisted immutable binding matches.
func (catalog Catalog) ResolveBinding(binding Binding) (Profile, error) {
	if err := binding.Validate(); err != nil {
		return Profile{}, err
	}
	key := bindingReference{
		reference:     reference{name: binding.Name, version: binding.Version},
		contentSHA256: binding.ContentSHA256,
		image:         binding.Image,
	}
	value, ok := catalog.profiles[key]
	if !ok {
		// Qualified built-in contracts can recover historical image/platform content from
		// the persisted hash without accepting a mutable reference or guessing a platform.
		for _, platform := range []Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}} {
			candidate, err := NewOpenCodeV1(binding.Image, platform)
			if err == nil && candidate.Binding() == binding {
				return candidate, nil
			}
		}
		return Profile{}, fmt.Errorf("%w: immutable %s/%s", ErrNotFound, binding.Name, binding.Version)
	}
	return cloneProfile(value), nil
}

// Binding returns the complete persisted identity for this profile.
func (profile Profile) Binding() Binding {
	return Binding{
		Name: profile.contract.Name, Version: profile.contract.Version,
		ContentSHA256: profile.contentSHA256, Image: profile.contract.Image,
	}
}

// Validate rejects incomplete bindings, mutable tags, and bare local image IDs.
func (binding Binding) Validate() error {
	if strings.TrimSpace(binding.Name) == "" || strings.TrimSpace(binding.Name) != binding.Name ||
		strings.TrimSpace(binding.Version) == "" || strings.TrimSpace(binding.Version) != binding.Version ||
		len(binding.ContentSHA256) != 64 || binding.ContentSHA256 != strings.ToLower(binding.ContentSHA256) {
		return invalid("immutable binding is incomplete")
	}
	if _, err := hex.DecodeString(binding.ContentSHA256); err != nil {
		return invalid("immutable binding content hash is invalid")
	}
	if !IsExactRegistryImage(binding.Image) {
		return invalid("immutable binding image must be a registry name with an exact sha256 digest")
	}
	return nil
}

// IsExactRegistryImage reports whether image is a registry reference pinned by sha256 digest.
func IsExactRegistryImage(image string) bool {
	return registryImagePattern.MatchString(image)
}

// IsSupportedPlatform reports whether platform is supported by opencode-acp/v1.
func IsSupportedPlatform(platform Platform) bool {
	return platform.OS == "linux" && (platform.Arch == "arm64" || platform.Arch == "amd64")
}

func cloneProfile(profile Profile) Profile {
	profile.contract = cloneContract(profile.contract)
	return profile
}

func cloneContract(contract Contract) Contract {
	contract.Command = slices.Clone(contract.Command)
	contract.Environment = slices.Clone(contract.Environment)
	contract.Mounts = slices.Clone(contract.Mounts)
	contract.Capabilities = slices.Clone(contract.Capabilities)
	contract.Tmpfs = slices.Clone(contract.Tmpfs)
	return contract
}

func validate(contract Contract) error {
	if contract.Name != "opencode-acp" || contract.Version != "v1" {
		return invalid("unsupported profile reference %q/%q", contract.Name, contract.Version)
	}
	if !registryImagePattern.MatchString(contract.Image) {
		return invalid("image must be a registry name with an exact sha256 digest")
	}
	if !IsSupportedPlatform(contract.Platform) {
		return invalid("unsupported platform %q/%q", contract.Platform.OS, contract.Platform.Arch)
	}
	if contract.User != (User{UID: 10001, GID: 10001}) {
		return invalid("user must be numeric non-root identity 10001:10001")
	}
	if !slices.Equal(contract.Command, []string{"acp"}) {
		return invalid("command must be exactly acp")
	}

	wantEnvironment := map[string]string{
		"HOME":                          "/home/opencode",
		"MISE_DATA_DIR":                 "/home/opencode/.local/share/mise",
		"OPENCODE_AUTH_CONTENT":         "{}",
		"OPENCODE_AUTO_SHARE":           "false",
		"OPENCODE_DISABLE_AUTOUPDATE":   "1",
		"OPENCODE_DISABLE_MODELS_FETCH": "true",
		"OPENCODE_DISABLE_SHARE":        "1",
		"TMPDIR":                        "/tmp/opencode",
		"XDG_CACHE_HOME":                "/home/opencode/.cache",
		"XDG_CONFIG_HOME":               "/home/opencode/.config",
		"XDG_DATA_HOME":                 "/home/opencode/.local/share",
		"XDG_STATE_HOME":                "/home/opencode/.local/state",
	}
	seenEnvironment := make(map[string]struct{}, len(contract.Environment))
	for _, variable := range contract.Environment {
		value, known := wantEnvironment[variable.Name]
		if !known {
			return invalid("unknown environment key %q", variable.Name)
		}
		if _, duplicate := seenEnvironment[variable.Name]; duplicate {
			return invalid("duplicate environment key %q", variable.Name)
		}
		if variable.Value != value {
			return invalid("environment key %q has an invalid value", variable.Name)
		}
		seenEnvironment[variable.Name] = struct{}{}
	}
	if len(seenEnvironment) != len(wantEnvironment) {
		return invalid("environment contract is incomplete")
	}

	wantMounts := map[string]string{
		"mise":      "/home/opencode/.local/share/mise",
		"state":     "/home/opencode/.local/share/opencode",
		"workspace": "/workspace",
	}
	seenMounts := make(map[string]struct{}, len(contract.Mounts))
	for _, mount := range contract.Mounts {
		wantPath, known := wantMounts[mount.Name]
		if !known {
			return invalid("unknown logical mount %q", mount.Name)
		}
		if _, duplicate := seenMounts[mount.Name]; duplicate {
			return invalid("duplicate logical mount %q", mount.Name)
		}
		if mount.Path != wantPath || !validAbsolutePath(mount.Path) {
			return invalid("logical mount %q has invalid path %q", mount.Name, mount.Path)
		}
		seenMounts[mount.Name] = struct{}{}
	}
	if len(seenMounts) != len(wantMounts) {
		return invalid("logical mount contract is incomplete")
	}

	wantCapabilities := map[string]struct{}{
		"session/list":   {},
		"session/load":   {},
		"session/resume": {},
	}
	seenCapabilities := make(map[string]struct{}, len(contract.Capabilities))
	for _, capability := range contract.Capabilities {
		if _, known := wantCapabilities[capability]; !known {
			return invalid("unknown ACP capability %q", capability)
		}
		if _, duplicate := seenCapabilities[capability]; duplicate {
			return invalid("duplicate ACP capability %q", capability)
		}
		seenCapabilities[capability] = struct{}{}
	}
	if len(seenCapabilities) != len(wantCapabilities) {
		return invalid("required ACP capability contract is incomplete")
	}

	wantTmpfs := map[string]Tmpfs{
		"/home/opencode/.cache":       {Path: "/home/opencode/.cache", SizeBytes: 64 << 20},
		"/home/opencode/.config":      {Path: "/home/opencode/.config", SizeBytes: 16 << 20},
		"/home/opencode/.local/state": {Path: "/home/opencode/.local/state", SizeBytes: 16 << 20},
		"/home/opencode/.opencode":    {Path: "/home/opencode/.opencode", SizeBytes: 16 << 20},
		"/tmp/opencode":               {Path: "/tmp/opencode", SizeBytes: 64 << 20, Executable: true},
	}
	seenTmpfs := make(map[string]struct{}, len(contract.Tmpfs))
	for _, tmpfs := range contract.Tmpfs {
		want, known := wantTmpfs[tmpfs.Path]
		if !known {
			return invalid("unknown tmpfs path %q", tmpfs.Path)
		}
		if _, duplicate := seenTmpfs[tmpfs.Path]; duplicate {
			return invalid("duplicate tmpfs path %q", tmpfs.Path)
		}
		if tmpfs != want || !validAbsolutePath(tmpfs.Path) {
			return invalid("tmpfs path %q has invalid size or executable flag", tmpfs.Path)
		}
		seenTmpfs[tmpfs.Path] = struct{}{}
	}
	if len(seenTmpfs) != len(wantTmpfs) {
		return invalid("tmpfs contract is incomplete")
	}

	paths := make([]string, 0, len(contract.Mounts)+len(contract.Tmpfs))
	for _, mount := range contract.Mounts {
		paths = append(paths, mount.Path)
	}
	for _, tmpfs := range contract.Tmpfs {
		paths = append(paths, tmpfs.Path)
	}
	for i, first := range paths {
		for _, second := range paths[i+1:] {
			if pathsOverlap(first, second) {
				return invalid("writable paths %q and %q overlap", first, second)
			}
		}
	}

	if contract.Workspace != "/workspace" {
		return invalid("workspace must be exactly /workspace")
	}
	if contract.MemoryBytes != 512<<20 {
		return invalid("memory limit must be exactly 512 MiB")
	}
	if contract.PIDsLimit != 128 {
		return invalid("PID limit must be exactly 128")
	}
	return nil
}

func validAbsolutePath(value string) bool {
	return strings.HasPrefix(value, "/") && value != "/" && path.Clean(value) == value
}

func pathsOverlap(first, second string) bool {
	return first == second || strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/")
}

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}
