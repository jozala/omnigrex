package execution

import (
	"path"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// DiagnosticSpec is a trusted, disposable protocol probe. It has no Participant
// attachments, retained state, provider credentials, or business MCP authority.
// Composition owns its commands and nonsecret environment, not repository input.
type DiagnosticSpec struct {
	Namespace            string
	Image                string
	Platform             Platform
	Command              []string
	Environment          []string
	TemporaryDirectories []TemporaryDirectory
	MemoryBytes          int64
}

func (spec DiagnosticSpec) Validate() error {
	if spec.Namespace == "" || len(spec.Namespace) > 128 {
		return ErrInvalidSpec
	}
	for _, char := range spec.Namespace {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return ErrInvalidSpec
		}
	}
	named, err := reference.ParseAnyReference(spec.Image)
	if err != nil {
		return ErrInvalidSpec
	}
	canonical, ok := named.(reference.Digested)
	if !ok || canonical.Digest().Algorithm() != digest.SHA256 || canonical.Digest().Validate() != nil {
		return ErrInvalidSpec
	}
	if _, tagged := named.(reference.Tagged); tagged {
		return ErrInvalidSpec
	}
	if spec.Platform.OS != "linux" || (spec.Platform.Architecture != "amd64" && spec.Platform.Architecture != "arm64") || spec.MemoryBytes < 64<<20 || spec.MemoryBytes > 2<<30 || len(spec.Command) == 0 || !strings.HasPrefix(spec.Command[0], "/") {
		return ErrInvalidSpec
	}
	seen := map[string]bool{}
	workspace := false
	for _, directory := range spec.TemporaryDirectories {
		if directory.Target == "/" || path.Clean(directory.Target) != directory.Target || !strings.HasPrefix(directory.Target, "/") || directory.SizeBytes < 1<<20 || directory.SizeBytes > 64<<20 || directory.Executable {
			return ErrInvalidSpec
		}
		for target := range seen {
			if target == directory.Target || strings.HasPrefix(target, directory.Target+"/") || strings.HasPrefix(directory.Target, target+"/") {
				return ErrInvalidSpec
			}
		}
		seen[directory.Target] = true
		workspace = workspace || directory.Target == "/workspace"
	}
	if !workspace {
		return ErrInvalidSpec
	}
	for _, argument := range spec.Command {
		if strings.ContainsRune(argument, 0) {
			return ErrInvalidSpec
		}
	}
	environment := map[string]bool{}
	for _, entry := range spec.Environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) || environment[name] {
			return ErrInvalidSpec
		}
		environment[name] = true
	}
	return nil
}
