package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

var ErrInvalidProcess = errors.New("invalid OpenCode Runtime Process")

type ProcessOptions struct {
	Key         execution.Key
	WorkflowID  string
	MemoryBytes int64
	Environment map[string]string
}

// ProviderCredentials is used in process memory only during OpenCode compatibility.
type ProviderCredentials struct {
	Role    Role
	Content []byte
}

// BuildProcess emits agent configuration and logical attachments. The execution
// backend owns storage materialization, management labels and security policy.
func BuildProcess(runtimeProfile profile.Profile, rendered *RenderedProfile, credentials ProviderCredentials, options ProcessOptions) (execution.Request, error) {
	contract := runtimeProfile.Contract()
	key := options.Key
	if rendered == nil || credentials.Role != rendered.role || key.AssignmentID == "" ||
		strings.Contains(key.AssignmentID, "/") || key.AssignmentID == "." || key.AssignmentID == ".." ||
		key.AgentSessionID == "" || key.AgentTurnID == "" || key.ExecutionEpoch <= 0 || options.MemoryBytes <= 0 ||
		contract.Name == "" || contract.Version == "" || key.RuntimeProfileName != contract.Name || key.RuntimeProfileVersion != contract.Version {
		return execution.Request{}, ErrInvalidProcess
	}
	authContent, err := providerAuthContent(credentials.Content)
	if err != nil {
		return execution.Request{}, err
	}
	environment, err := compileEnvironment(contract.Environment, rendered.Environment())
	if err != nil {
		return execution.Request{}, err
	}
	for name, value := range options.Environment {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
			return execution.Request{}, fmt.Errorf("%w: invalid trusted tool environment", ErrInvalidProcess)
		}
		if existing, present := environment[name]; present && existing != value {
			return execution.Request{}, fmt.Errorf("%w: environment values conflict for %s", ErrInvalidProcess, name)
		}
		environment[name] = value
	}
	if _, present := environment["TMPDIR"]; !present {
		environment["TMPDIR"] = "/tmp/opencode"
	}
	if environment["OPENCODE_AUTH_CONTENT"] != "{}" {
		return execution.Request{}, fmt.Errorf("%w: static OpenCode authentication placeholder is invalid", ErrInvalidProcess)
	}
	environment["OPENCODE_AUTH_CONTENT"] = authContent
	request := execution.Request{
		Key: key, WorkflowID: options.WorkflowID, Image: contract.Image,
		Platform:   execution.Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch},
		User:       strconv.FormatUint(uint64(contract.User.UID), 10) + ":" + strconv.FormatUint(uint64(contract.User.GID), 10),
		WorkingDir: contract.Workspace, Command: slices.Clone(contract.Command), Environment: environmentEntries(environment),
		MemoryBytes: options.MemoryBytes, PIDsLimit: contract.PIDsLimit,
	}
	for _, mount := range contract.Mounts {
		request.Attachments = append(request.Attachments, execution.Attachment{Resource: mount.Name, Target: mount.Path})
	}
	for _, temporary := range contract.Tmpfs {
		request.TemporaryDirectories = append(request.TemporaryDirectories, execution.TemporaryDirectory{
			Target: temporary.Path, SizeBytes: temporary.SizeBytes, Executable: temporary.Executable,
		})
	}
	return request, nil
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
