package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"slices"
	"sort"
	"strings"

	distribution "github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
)

var (
	ErrInvalid  = errors.New("invalid Runtime Profile")
	ErrConflict = errors.New("Runtime Profile reference conflict")
	ErrNotFound = errors.New("Runtime Profile not found")
)

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
	Name                   string                 `json:"name"`
	Version                string                 `json:"version"`
	Image                  string                 `json:"image"`
	Platform               Platform               `json:"platform"`
	User                   User                   `json:"user"`
	Command                []string               `json:"command"`
	Environment            []EnvironmentVariable  `json:"environment"`
	Mounts                 []Mount                `json:"mounts"`
	Capabilities           []string               `json:"capabilities"`
	Tmpfs                  []Tmpfs                `json:"tmpfs"`
	Workspace              string                 `json:"workspace"`
	PIDsLimit              int64                  `json:"pids_limit"`
	RepositoryEnvironment  *RepositoryEnvironment `json:"repository_environment,omitempty"`
	GoldenResourceContract json.RawMessage        `json:"golden_resource_contract,omitempty"`
}

// RepositoryEnvironment binds the golden resource contract and backend guarantees.
type RepositoryEnvironment struct {
	Version      int      `json:"version"`
	Requirements []string `json:"requirements"`
}

func (profile Profile) WithEnvironment(image string, platform Platform, requirements []string) (Profile, error) {
	contract := profile.Contract()
	if contract.Name == "pi-rpc" && platform != contract.Platform {
		// This selects a fresh Participant, not a retained binding. Capture the
		// deployment's approved bundle for the newly selected guest platform.
		contract.GoldenResourceContract = piresources.CurrentContract(platform.Arch)
	}
	contract.Image, contract.Platform = image, platform
	contract.RepositoryEnvironment = &RepositoryEnvironment{Version: 1, Requirements: slices.Clone(requirements)}
	return New(contract)
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

// New validates and snapshots a Pi Runtime Profile contract.
func New(contract Contract) (Profile, error) {
	if err := validate(contract); err != nil {
		return Profile{}, err
	}
	contract = normalizeContract(contract)
	return newProfile(contract)
}

func normalizeContract(contract Contract) Contract {
	contract = cloneContract(contract)
	if len(contract.GoldenResourceContract) > 0 {
		contract.GoldenResourceContract, _ = piresources.NormalizeContract(contract.GoldenResourceContract)
	}
	sort.Slice(contract.Environment, func(i, j int) bool { return contract.Environment[i].Name < contract.Environment[j].Name })
	sort.Slice(contract.Mounts, func(i, j int) bool { return contract.Mounts[i].Name < contract.Mounts[j].Name })
	slices.Sort(contract.Capabilities)
	if contract.RepositoryEnvironment != nil {
		slices.Sort(contract.RepositoryEnvironment.Requirements)
	}
	sort.Slice(contract.Tmpfs, func(i, j int) bool { return contract.Tmpfs[i].Path < contract.Tmpfs[j].Path })
	return contract
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
	named, err := distribution.ParseNamed(image)
	if err != nil || named.String() != image {
		return false
	}
	canonical, ok := named.(distribution.Canonical)
	if !ok {
		return false
	}
	if _, tagged := named.(distribution.Tagged); tagged {
		return false
	}
	imageDigest := canonical.Digest()
	return imageDigest.Algorithm() == digest.SHA256 && imageDigest.Validate() == nil
}

// IsSupportedPlatform reports whether platform is supported by Pi.
func IsSupportedPlatform(platform Platform) bool {
	return platform.OS == "linux" && (platform.Arch == "arm64" || platform.Arch == "amd64")
}

func cloneProfile(profile Profile) Profile {
	profile.contract = cloneContract(profile.contract)
	return profile
}

func cloneContract(contract Contract) Contract {
	contract.GoldenResourceContract = slices.Clone(contract.GoldenResourceContract)
	contract.Command = slices.Clone(contract.Command)
	contract.Environment = slices.Clone(contract.Environment)
	contract.Mounts = slices.Clone(contract.Mounts)
	contract.Capabilities = slices.Clone(contract.Capabilities)
	contract.Tmpfs = slices.Clone(contract.Tmpfs)
	if contract.RepositoryEnvironment != nil {
		environment := *contract.RepositoryEnvironment
		environment.Requirements = slices.Clone(environment.Requirements)
		contract.RepositoryEnvironment = &environment
	}
	return contract
}

func validate(contract Contract) error {
	if environment := contract.RepositoryEnvironment; environment != nil {
		if environment.Version != 1 {
			return invalid("unsupported repository environment version")
		}
		seen := map[string]bool{}
		for _, requirement := range environment.Requirements {
			if seen[requirement] || !slices.Contains([]string{"process-isolation", "hardware-isolation", "nested-containers", "tool-installation"}, requirement) {
				return invalid("invalid environment requirement %q", requirement)
			}
			seen[requirement] = true
		}
	}
	if contract.Name == "pi-rpc" && contract.Version == "v1" {
		return validatePi(contract)
	}
	return invalid("unsupported profile reference %q/%q", contract.Name, contract.Version)
}

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}
