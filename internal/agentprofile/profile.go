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

	"github.com/jozala/omnigrex/internal/role"
	"gopkg.in/yaml.v3"
)

const MaxContentSize = 256 << 10

var (
	ErrInvalidProfile  = errors.New("invalid Agent Profile")
	ErrProfileTooLarge = errors.New("Agent Profile exceeds 256 KiB")
	ErrUnknownProfile  = errors.New("unknown Agent Profile")
)

type Name string

type Role = role.ID

type PermissionAction string

const (
	Allow PermissionAction = "allow"
	Deny  PermissionAction = "deny"
)

type Profile struct {
	name         Name
	role         Role
	path         string
	runtime      string
	model        string
	thinking     string
	permissions  map[string]PermissionAction
	instructions string
	contentHash  [sha256.Size]byte
	canonical    []byte
}

type frontMatter struct {
	Name        Name                        `yaml:"name"`
	Role        Role                        `yaml:"role"`
	Runtime     string                      `yaml:"runtime"`
	Model       string                      `yaml:"model"`
	Thinking    string                      `yaml:"thinking,omitempty"`
	Permissions map[string]PermissionAction `yaml:"permissions"`
}

func Parse(path string, content []byte, policies role.PolicyCatalog) (Profile, error) {
	if !validPath(path) {
		return Profile{}, fmt.Errorf("%w: path must be a direct .md file under .omnigrex/team", ErrInvalidProfile)
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
	return profileFromConfiguration(path, configuration, instructions, sha256.Sum256(content), policies)
}

func profileFromConfiguration(path string, configuration frontMatter, instructions []byte, contentHash [sha256.Size]byte, policies role.PolicyCatalog) (Profile, error) {
	if !validPath(path) {
		return Profile{}, ErrInvalidProfile
	}
	for _, value := range []string{string(configuration.Name), string(configuration.Role), configuration.Runtime, configuration.Model, configuration.Thinking, string(instructions)} {
		if strings.ContainsRune(value, '\ufeff') {
			return Profile{}, ErrInvalidProfile
		}
	}
	if !validName(configuration.Name) {
		return Profile{}, fmt.Errorf("%w: name must use safe lowercase syntax", ErrInvalidProfile)
	}
	policy, ok := policies.Lookup(configuration.Role)
	if !ok {
		return Profile{}, fmt.Errorf("%w: Role %q has no policy", ErrInvalidProfile, configuration.Role)
	}
	if err := validateConfiguration(policy, configuration, instructions); err != nil {
		return Profile{}, err
	}

	profile := Profile{
		name:         configuration.Name,
		role:         configuration.Role,
		path:         path,
		runtime:      configuration.Runtime,
		model:        configuration.Model,
		thinking:     configuration.Thinking,
		permissions:  clonePermissions(configuration.Permissions),
		instructions: string(instructions),
		contentHash:  contentHash,
	}
	var err error
	profile.canonical, err = json.Marshal(struct {
		Instructions string                      `json:"instructions"`
		Model        string                      `json:"model"`
		Name         Name                        `json:"name"`
		Path         string                      `json:"path"`
		Permissions  map[string]PermissionAction `json:"permissions"`
		Role         Role                        `json:"role"`
		Runtime      string                      `json:"runtime"`
		Thinking     string                      `json:"thinking,omitempty"`
	}{
		Instructions: profile.instructions,
		Model:        profile.model,
		Name:         profile.name,
		Path:         profile.path,
		Permissions:  profile.permissions,
		Role:         profile.role,
		Runtime:      profile.runtime,
		Thinking:     profile.thinking,
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
func (profile Profile) Thinking() string                 { return profile.thinking }
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

func validateConfiguration(policy role.Policy, configuration frontMatter, instructions []byte) error {
	if configuration.Runtime != "pi-rpc/v2" {
		return fmt.Errorf("%w: runtime must be pi-rpc/v2", ErrInvalidProfile)
	}
	provider, model, found := strings.Cut(configuration.Model, "/")
	if !found || provider == "" || model == "" || containsWhitespaceOrControl(configuration.Model) {
		return fmt.Errorf("%w: model must use provider/model syntax without whitespace or control characters", ErrInvalidProfile)
	}
	if configuration.Thinking != "" && !ValidThinking(configuration.Thinking) {
		return fmt.Errorf("%w: invalid Pi thinking level", ErrInvalidProfile)
	}
	if err := ValidatePiPermissions(policy, configuration.Permissions); err != nil {
		return err
	}
	if strings.TrimSpace(string(instructions)) == "" {
		return fmt.Errorf("%w: Role instructions must not be blank", ErrInvalidProfile)
	}
	if !utf8.Valid(instructions) {
		return fmt.Errorf("%w: Role instructions must be valid UTF-8", ErrInvalidProfile)
	}
	return nil
}

// ValidThinking checks vocabulary only; the adapter verifies model support
// against the actual Pi runtime. max and xhigh are distinct levels.
func ValidThinking(value string) bool {
	switch value {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// ValidatePiPermissions applies immutable Role constraints to Pi's local tools.
// Skill authorization adds checkout-local guidance, not another tool authority.
func ValidatePiPermissions(policy role.Policy, permissions map[string]PermissionAction) error {
	if !role.ValidID(policy.Role) || len(permissions) == 0 {
		return ErrInvalidProfile
	}
	for tool, action := range permissions {
		switch tool {
		case "read", "edit", "write", "bash", "grep", "find", "ls", "skill":
		default:
			return fmt.Errorf("%w: unsupported Pi local tool", ErrInvalidProfile)
		}
		if action != Allow && action != Deny || !policy.AllowFileEdits && action == Allow && (tool == "edit" || tool == "write") {
			return fmt.Errorf("%w: invalid Pi Role permission", ErrInvalidProfile)
		}
	}
	return nil
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
