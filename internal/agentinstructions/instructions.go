// Package agentinstructions composes current-deployment guidance without persisting it as session authority.
package agentinstructions

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	maxFileBytes         = 64 << 10
	maxOperatorJSONBytes = 64 << 10
)

//go:embed platform.md
var platform string

// Operator is an immutable startup-loaded set of deployment instructions.
// Its zero value adds no operator guidance.
type Operator struct {
	common string
	roles  map[role.ID]string
}

// Load reads optional common.md and roles/<ROLE>.md files once, validating them against the deployment catalog.
func Load(directory string, policies role.PolicyCatalog) (Operator, error) {
	if directory == "" {
		return Operator{}, nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return Operator{}, fmt.Errorf("read agent instructions directory: %w", err)
	}
	result := Operator{roles: make(map[role.ID]string)}
	for _, entry := range entries {
		switch entry.Name() {
		case "common.md":
			result.common, err = readFile(filepath.Join(directory, entry.Name()))
		case "roles":
			var files []os.DirEntry
			files, err = os.ReadDir(filepath.Join(directory, entry.Name()))
			if err != nil {
				break
			}
			for _, file := range files {
				id := role.ID(strings.TrimSuffix(file.Name(), ".md"))
				if _, known := policies.Lookup(id); !known || file.Name() != string(id)+".md" {
					return Operator{}, fmt.Errorf("unknown agent instructions Role file %q", file.Name())
				}
				result.roles[id], err = readFile(filepath.Join(directory, "roles", file.Name()))
				if err != nil {
					break
				}
			}
		default:
			return Operator{}, fmt.Errorf("unexpected agent instructions entry %q", entry.Name())
		}
		if err != nil {
			return Operator{}, fmt.Errorf("load agent instructions %s: %w", entry.Name(), err)
		}
	}
	// OpenCode carries its configuration in one environment string. Bound the
	// escaped operator contribution as well as each file's raw bytes, leaving
	// room for platform, Stage, Profile, and tool configuration. The runtime
	// renderer separately checks the complete serialized configuration.
	for _, id := range policies.Roles() {
		encoded, err := json.Marshal(result.common + "\n\n" + result.roles[id])
		if err != nil || len(encoded) > maxOperatorJSONBytes {
			return Operator{}, fmt.Errorf("combined common and %s instructions exceed the %d-byte JSON-encoded budget", id, maxOperatorJSONBytes)
		}
	}
	return result, nil
}

func readFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", filepath.Base(path))
	}
	content, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxFileBytes || !utf8.Valid(content) || strings.ContainsRune(string(content), '\x00') {
		return "", fmt.Errorf("%s must be UTF-8 text without NUL and at most %d bytes", filepath.Base(path), maxFileBytes)
	}
	return strings.TrimSpace(string(content)), nil
}

// Compose uses the running binary's platform/Stage guidance and startup-loaded operator files.
// Repository Profile provenance is separate; never reload a composed historical prompt here.
func (operator Operator) Compose(policy role.Policy, stage workflow.StageDefinition, purpose workflow.TurnPurpose, repositoryInstructions string) string {
	sections := []string{strings.TrimSpace(platform)}
	appendSection := func(title, content string) {
		if strings.TrimSpace(content) != "" {
			sections = append(sections, "## "+title+"\n\n"+strings.TrimSpace(content))
		}
	}
	if !policy.OpenCode.AllowFileEdits {
		appendSection("Role workspace constraints", "Inspect and verify without intentionally modifying tracked repository files.")
	}
	if policy.DiscardWorkspace {
		appendSection("Workspace lifetime", "Verification may create temporary files; Omnigrex discards workspace changes after the Agent Turn.")
	}
	appendSection("Current Stage: "+string(stage.ID), stage.Instructions)
	appendSection("Current Turn purpose: "+string(purpose), stage.PurposeInstructions[purpose])
	appendSection("Operator common guidance", operator.common)
	appendSection("Operator Role guidance: "+string(policy.Role), operator.roles[policy.Role])
	appendSection("Repository Agent Profile", repositoryInstructions)
	return strings.Join(sections, "\n\n")
}
