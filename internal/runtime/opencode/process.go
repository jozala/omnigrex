package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

var ErrInvalidProcess = errors.New("invalid OpenCode Runtime Process")

const (
	assignmentLabel     = "io.omnigrex.assignment"
	agentSessionLabel   = "io.omnigrex.agent-session"
	agentTurnLabel      = "io.omnigrex.agent-turn"
	executionEpochLabel = "io.omnigrex.execution-epoch"
	runtimeProfileLabel = "io.omnigrex.runtime-profile"
)

var reservedProcessLabels = map[string]struct{}{
	assignmentLabel:     {},
	agentSessionLabel:   {},
	agentTurnLabel:      {},
	executionEpochLabel: {},
	runtimeProfileLabel: {},
}

// ProcessOptions binds immutable Runtime Profile paths to one deployment and Assignment.
type ProcessOptions struct {
	Name               string
	Network            string
	AssignmentID       string
	AgentSessionID     string
	AgentTurnID        string
	ExecutionEpoch     uint64
	VolumeBindings     map[string]string
	AssignmentSubpaths map[string]string
	Labels             map[string]string
}

// ProviderCredentials carries one Role's OpenCode authentication object in process memory only.
type ProviderCredentials struct {
	Role    Role
	Content []byte
}

// BuildProcess compiles a validated Runtime Profile and rendered Role configuration into an exact Docker contract.
func BuildProcess(runtimeProfile profile.Profile, rendered *RenderedProfile, credentials ProviderCredentials, options ProcessOptions) (dockerruntime.RuntimePolicy, dockerruntime.Spec, error) {
	contract := runtimeProfile.Contract()
	if rendered == nil || credentials.Role != rendered.role || !validProcessIdentity(options) || contract.Name == "" || contract.Version == "" {
		return dockerruntime.RuntimePolicy{}, dockerruntime.Spec{}, ErrInvalidProcess
	}

	authContent, err := providerAuthContent(credentials.Content)
	if err != nil {
		return dockerruntime.RuntimePolicy{}, dockerruntime.Spec{}, err
	}
	environment, err := compileEnvironment(contract.Environment, rendered.Environment())
	if err != nil {
		return dockerruntime.RuntimePolicy{}, dockerruntime.Spec{}, err
	}
	if environment["OPENCODE_AUTH_CONTENT"] != "{}" {
		return dockerruntime.RuntimePolicy{}, dockerruntime.Spec{}, fmt.Errorf("%w: static OpenCode authentication placeholder is invalid", ErrInvalidProcess)
	}
	environment["OPENCODE_AUTH_CONTENT"] = authContent
	entries := environmentEntries(environment)
	labels, err := compileLabels(contract, options)
	if err != nil {
		return dockerruntime.RuntimePolicy{}, dockerruntime.Spec{}, err
	}
	volumes, err := compileVolumes(contract.Mounts, options)
	if err != nil {
		return dockerruntime.RuntimePolicy{}, dockerruntime.Spec{}, err
	}
	tmpfs := make([]dockerruntime.TmpfsMount, len(contract.Tmpfs))
	for index, temporary := range contract.Tmpfs {
		tmpfs[index] = dockerruntime.TmpfsMount{
			Target: temporary.Path, SizeBytes: temporary.SizeBytes, Executable: temporary.Executable,
		}
	}
	platform := dockerruntime.Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch}
	user := strconv.FormatUint(uint64(contract.User.UID), 10) + ":" + strconv.FormatUint(uint64(contract.User.GID), 10)

	spec := dockerruntime.Spec{
		Name:        options.Name,
		Image:       contract.Image,
		Platform:    platform,
		User:        user,
		WorkingDir:  contract.Workspace,
		Command:     slices.Clone(contract.Command),
		Environment: entries,
		Labels:      maps.Clone(labels),
		Volumes:     slices.Clone(volumes),
		Tmpfs:       slices.Clone(tmpfs),
		Network:     options.Network,
		MemoryBytes: contract.MemoryBytes,
		PIDsLimit:   contract.PIDsLimit,
	}
	policy := dockerruntime.RuntimePolicy{
		Image:       contract.Image,
		Platform:    platform,
		User:        user,
		WorkingDir:  contract.Workspace,
		Command:     slices.Clone(contract.Command),
		Volumes:     slices.Clone(volumes),
		Tmpfs:       slices.Clone(tmpfs),
		Environment: maps.Clone(environment),
		Labels:      maps.Clone(labels),
		Network:     options.Network,
		MemoryBytes: contract.MemoryBytes,
		PIDsLimit:   contract.PIDsLimit,
	}
	return policy, spec, nil
}

func providerAuthContent(content []byte) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(content, &object); err != nil || object == nil {
		return "", fmt.Errorf("%w: provider credentials must be a JSON object", ErrInvalidProcess)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		return "", fmt.Errorf("%w: provider credentials must be a JSON object", ErrInvalidProcess)
	}
	return compact.String(), nil
}

func validProcessIdentity(options ProcessOptions) bool {
	for _, value := range []string{options.Name, options.Network, options.AssignmentID, options.AgentSessionID, options.AgentTurnID} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return false
		}
	}
	unsafeNetwork := options.Network == "host" || options.Network == "default" || strings.HasPrefix(options.Network, "container:")
	return options.ExecutionEpoch != 0 && !unsafeNetwork && !strings.Contains(options.AssignmentID, "/") && options.AssignmentID != "." && options.AssignmentID != ".."
}

func compileEnvironment(static []profile.EnvironmentVariable, dynamic []string) (map[string]string, error) {
	environment := make(map[string]string, len(static)+len(dynamic))
	merge := func(name, value string) error {
		if name == "" {
			return ErrInvalidProcess
		}
		if existing, present := environment[name]; present && existing != value {
			return fmt.Errorf("%w: environment values conflict for %s", ErrInvalidProcess, name)
		}
		environment[name] = value
		return nil
	}
	for _, variable := range static {
		if err := merge(variable.Name, variable.Value); err != nil {
			return nil, err
		}
	}
	for _, entry := range dynamic {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			return nil, ErrInvalidProcess
		}
		if err := merge(name, value); err != nil {
			return nil, err
		}
	}
	if _, present := environment["OPENCODE_CONFIG_CONTENT"]; !present {
		return nil, fmt.Errorf("%w: OpenCode configuration is missing", ErrInvalidProcess)
	}
	return environment, nil
}

func environmentEntries(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, name+"="+environment[name])
	}
	return entries
}

func compileVolumes(mounts []profile.Mount, options ProcessOptions) ([]dockerruntime.VolumeMount, error) {
	if len(options.VolumeBindings) != len(mounts) || len(options.AssignmentSubpaths) != len(mounts) {
		return nil, fmt.Errorf("%w: logical volume mappings are incomplete", ErrInvalidProcess)
	}
	assignmentRoot := "assignment-" + options.AssignmentID
	volumes := make([]dockerruntime.VolumeMount, 0, len(mounts))
	for _, mount := range mounts {
		volumeName, bound := options.VolumeBindings[mount.Name]
		subpath, isolated := options.AssignmentSubpaths[mount.Name]
		if !bound || !isolated || strings.TrimSpace(volumeName) == "" || strings.TrimSpace(volumeName) != volumeName ||
			path.IsAbs(subpath) || path.Clean(subpath) != subpath || !strings.HasPrefix(subpath, assignmentRoot+"/") {
			return nil, fmt.Errorf("%w: logical volume mapping is invalid", ErrInvalidProcess)
		}
		volumes = append(volumes, dockerruntime.VolumeMount{Name: volumeName, Subpath: subpath, Target: mount.Path})
	}
	for index, volume := range volumes {
		for _, other := range volumes[index+1:] {
			if volume.Name == other.Name && pathsOverlap(volume.Subpath, other.Subpath) {
				return nil, fmt.Errorf("%w: Assignment volume subpaths overlap", ErrInvalidProcess)
			}
		}
	}
	return volumes, nil
}

func compileLabels(contract profile.Contract, options ProcessOptions) (map[string]string, error) {
	labels := make(map[string]string, len(options.Labels)+len(reservedProcessLabels))
	for name, value := range options.Labels {
		lowerName := strings.ToLower(name)
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("%w: invalid nonsecret label", ErrInvalidProcess)
		}
		if _, reserved := reservedProcessLabels[name]; reserved {
			return nil, fmt.Errorf("%w: reserved label override", ErrInvalidProcess)
		}
		for _, forbidden := range []string{"owner", "lease", "token", "credential", "secret"} {
			if strings.Contains(lowerName, forbidden) {
				return nil, fmt.Errorf("%w: sensitive label keys are forbidden", ErrInvalidProcess)
			}
		}
		labels[name] = value
	}
	labels[assignmentLabel] = options.AssignmentID
	labels[agentSessionLabel] = options.AgentSessionID
	labels[agentTurnLabel] = options.AgentTurnID
	labels[executionEpochLabel] = strconv.FormatUint(options.ExecutionEpoch, 10)
	labels[runtimeProfileLabel] = contract.Name + "/" + contract.Version
	return labels, nil
}

func pathsOverlap(first, second string) bool {
	return first == second || strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/")
}
