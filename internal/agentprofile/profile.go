package agentprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const MaxContentSize = 256 << 10

var (
	ErrInvalidProfile  = errors.New("invalid Agent Profile")
	ErrProfileTooLarge = errors.New("Agent Profile exceeds 256 KiB")
	ErrUnknownProfile  = errors.New("unknown Agent Profile")
)

type Name string

const (
	Developer Name = "developer"
	Reviewer  Name = "reviewer"
)

type Role string

const (
	RoleDeveloper Role = "DEVELOPER"
	RoleReviewer  Role = "REVIEWER"
)

type PermissionAction string

const (
	Allow PermissionAction = "allow"
	Deny  PermissionAction = "deny"
)

var knownTools = map[string]struct{}{
	"read": {}, "edit": {}, "glob": {}, "grep": {}, "list": {}, "patch": {},
	"bash": {}, "task": {}, "webfetch": {}, "websearch": {}, "codesearch": {},
	"todoread": {}, "todowrite": {}, "question": {}, "skill": {},
}

type Profile struct {
	name         Name
	role         Role
	path         string
	runtime      string
	model        string
	variant      string
	steps        int
	permissions  map[string]PermissionAction
	instructions string
	contentHash  [sha256.Size]byte
	canonical    []byte
}

type frontMatter struct {
	Runtime     string                      `yaml:"runtime"`
	Model       string                      `yaml:"model"`
	Variant     string                      `yaml:"variant,omitempty"`
	Steps       int                         `yaml:"steps"`
	Permissions map[string]PermissionAction `yaml:"permissions"`
}

func Parse(name Name, content []byte) (Profile, error) {
	identity, err := identityFor(name)
	if err != nil {
		return Profile{}, err
	}
	if len(content) > MaxContentSize {
		return Profile{}, ErrProfileTooLarge
	}
	if bytes.Contains(content, []byte{0xef, 0xbb, 0xbf}) {
		return Profile{}, fmt.Errorf("%w: UTF-8 BOM is not allowed", ErrInvalidProfile)
	}
	front, instructions, err := splitDocument(content)
	if err != nil {
		return Profile{}, err
	}
	configuration, err := decodeFrontMatter(front)
	if err != nil {
		return Profile{}, err
	}
	if err := validateConfiguration(configuration, instructions); err != nil {
		return Profile{}, err
	}

	profile := Profile{
		name:         name,
		role:         identity.role,
		path:         identity.path,
		runtime:      configuration.Runtime,
		model:        configuration.Model,
		variant:      configuration.Variant,
		steps:        configuration.Steps,
		permissions:  clonePermissions(configuration.Permissions),
		instructions: string(instructions),
		contentHash:  sha256.Sum256(content),
	}
	profile.canonical, err = json.Marshal(struct {
		Instructions string                      `json:"instructions"`
		Model        string                      `json:"model"`
		Name         Name                        `json:"name"`
		Path         string                      `json:"path"`
		Permissions  map[string]PermissionAction `json:"permissions"`
		Role         Role                        `json:"role"`
		Runtime      string                      `json:"runtime"`
		Steps        int                         `json:"steps"`
		Variant      string                      `json:"variant,omitempty"`
	}{
		Instructions: profile.instructions,
		Model:        profile.model,
		Name:         profile.name,
		Path:         profile.path,
		Permissions:  profile.permissions,
		Role:         profile.role,
		Runtime:      profile.runtime,
		Steps:        profile.steps,
		Variant:      profile.variant,
	})
	if err != nil {
		return Profile{}, fmt.Errorf("encode Agent Profile snapshot: %w", err)
	}
	return profile, nil
}

func (profile Profile) Name() Name                       { return profile.name }
func (profile Profile) Role() Role                       { return profile.role }
func (profile Profile) Path() string                     { return profile.path }
func (profile Profile) Runtime() string                  { return profile.runtime }
func (profile Profile) Model() string                    { return profile.model }
func (profile Profile) Variant() string                  { return profile.variant }
func (profile Profile) Steps() int                       { return profile.steps }
func (profile Profile) Instructions() string             { return profile.instructions }
func (profile Profile) ContentSHA256() [sha256.Size]byte { return profile.contentHash }
func (profile Profile) ContentHash() string              { return hex.EncodeToString(profile.contentHash[:]) }
func (profile Profile) Permissions() map[string]PermissionAction {
	return clonePermissions(profile.permissions)
}
func (profile Profile) CanonicalJSON() []byte { return bytes.Clone(profile.canonical) }

func (profile Profile) Permission(tool string) PermissionAction {
	if action, exists := profile.permissions[tool]; exists {
		return action
	}
	return Deny
}

type profileIdentity struct {
	role Role
	path string
}

func identityFor(name Name) (profileIdentity, error) {
	switch name {
	case Developer:
		return profileIdentity{role: RoleDeveloper, path: ".omnigrex/team/developer.md"}, nil
	case Reviewer:
		return profileIdentity{role: RoleReviewer, path: ".omnigrex/team/reviewer.md"}, nil
	default:
		return profileIdentity{}, fmt.Errorf("%w: %q", ErrUnknownProfile, name)
	}
}

func splitDocument(content []byte) ([]byte, []byte, error) {
	frontStart := 0
	switch {
	case bytes.HasPrefix(content, []byte("---\r\n")):
		frontStart = len("---\r\n")
	case bytes.HasPrefix(content, []byte("---\n")):
		frontStart = len("---\n")
	default:
		return nil, nil, fmt.Errorf("%w: front matter must start with an exact --- delimiter", ErrInvalidProfile)
	}
	lineStart := frontStart
	for lineStart < len(content) {
		lineEndOffset := bytes.IndexByte(content[lineStart:], '\n')
		if lineEndOffset < 0 {
			break
		}
		lineEnd := lineStart + lineEndOffset
		line := content[lineStart:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if bytes.Equal(line, []byte("---")) {
			return content[frontStart:lineStart], content[lineEnd+1:], nil
		}
		lineStart = lineEnd + 1
	}
	return nil, nil, fmt.Errorf("%w: front matter must end with an exact --- delimiter", ErrInvalidProfile)
}

func decodeFrontMatter(content []byte) (frontMatter, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&document); err != nil {
		return frontMatter{}, fmt.Errorf("%w: decode YAML front matter: %v", ErrInvalidProfile, err)
	}
	if err := ensureSingleYAMLDocument(decoder); err != nil {
		return frontMatter{}, err
	}
	if err := validateYAMLNode(&document); err != nil {
		return frontMatter{}, err
	}

	var configuration frontMatter
	strict := yaml.NewDecoder(bytes.NewReader(content))
	strict.KnownFields(true)
	if err := strict.Decode(&configuration); err != nil {
		return frontMatter{}, fmt.Errorf("%w: decode YAML front matter: %v", ErrInvalidProfile, err)
	}
	if err := ensureSingleYAMLDocument(strict); err != nil {
		return frontMatter{}, err
	}
	return configuration, nil
}

func ensureSingleYAMLDocument(decoder *yaml.Decoder) error {
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%w: YAML front matter contains multiple documents", ErrInvalidProfile)
		}
		return fmt.Errorf("%w: decode YAML front matter: %v", ErrInvalidProfile, err)
	}
	return nil
}

func validateYAMLNode(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("%w: YAML aliases and anchors are not allowed", ErrInvalidProfile)
	}
	if node.Tag != "" && node.Tag != "!!map" && node.Tag != "!!str" && node.Tag != "!!int" {
		return fmt.Errorf("%w: YAML tag %q is not allowed", ErrInvalidProfile, node.Tag)
	}
	for _, child := range node.Content {
		if err := validateYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func validateConfiguration(configuration frontMatter, instructions []byte) error {
	if !validReference(configuration.Runtime) {
		return fmt.Errorf("%w: runtime must use name/version syntax without whitespace or control characters", ErrInvalidProfile)
	}
	if !validReference(configuration.Model) {
		return fmt.Errorf("%w: model must use provider/model syntax without whitespace or control characters", ErrInvalidProfile)
	}
	if configuration.Variant != "" && containsWhitespaceOrControl(configuration.Variant) {
		return fmt.Errorf("%w: variant contains whitespace or control characters", ErrInvalidProfile)
	}
	if configuration.Steps <= 0 || configuration.Steps > 1000 {
		return fmt.Errorf("%w: steps must be between 1 and 1000", ErrInvalidProfile)
	}
	if len(configuration.Permissions) == 0 {
		return fmt.Errorf("%w: permissions must not be empty", ErrInvalidProfile)
	}
	for tool, action := range configuration.Permissions {
		if _, ok := knownTools[tool]; !ok {
			return fmt.Errorf("%w: unknown permission tool %q", ErrInvalidProfile, tool)
		}
		if action != Allow && action != Deny {
			return fmt.Errorf("%w: permission for %q must be allow or deny", ErrInvalidProfile, tool)
		}
	}
	if strings.TrimSpace(string(instructions)) == "" {
		return fmt.Errorf("%w: Role instructions must not be blank", ErrInvalidProfile)
	}
	if !utf8.Valid(instructions) {
		return fmt.Errorf("%w: Role instructions must be valid UTF-8", ErrInvalidProfile)
	}
	return nil
}

func validReference(value string) bool {
	if strings.Count(value, "/") != 1 || containsWhitespaceOrControl(value) {
		return false
	}
	parts := strings.Split(value, "/")
	return parts[0] != "" && parts[1] != ""
}

func containsWhitespaceOrControl(value string) bool {
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func clonePermissions(source map[string]PermissionAction) map[string]PermissionAction {
	clone := make(map[string]PermissionAction, len(source))
	for tool, action := range source {
		clone[tool] = action
	}
	return clone
}
