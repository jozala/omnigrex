package agentprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/jozala/omnigrex/internal/role"
)

// JSON can encode one source byte as six bytes. The additional bounded room
// accommodates canonical field names and the separately supplied Profile path.
const maxSnapshotBytes = 8 * MaxContentSize

// DecodeSnapshot validates retained canonical inputs through the same Profile
// parser as repository discovery, including presence-sensitive Pi exclusions.
func DecodeSnapshot(raw json.RawMessage, policies role.PolicyCatalog) (Profile, error) {
	var fields map[string]json.RawMessage
	if len(raw) > maxSnapshotBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &fields) != nil {
		return Profile{}, ErrInvalidProfile
	}
	for key := range fields {
		switch key {
		case "name", "role", "path", "runtime", "model", "variant", "thinking", "steps", "permissions", "instructions":
		default:
			return Profile{}, ErrInvalidProfile
		}
	}
	var value struct {
		Name         Name                        `json:"name"`
		Role         Role                        `json:"role"`
		Path         string                      `json:"path"`
		Runtime      string                      `json:"runtime"`
		Model        string                      `json:"model"`
		Variant      string                      `json:"variant,omitempty"`
		Thinking     string                      `json:"thinking,omitempty"`
		Steps        int                         `json:"steps,omitempty"`
		Permissions  map[string]PermissionAction `json:"permissions"`
		Instructions string                      `json:"instructions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return Profile{}, ErrInvalidProfile
	}
	if value.Runtime == "pi-rpc/v1" {
		if _, exists := fields["steps"]; exists {
			return Profile{}, ErrInvalidProfile
		}
		if _, exists := fields["variant"]; exists {
			return Profile{}, ErrInvalidProfile
		}
	}
	decodedBytes := len(value.Name) + len(value.Role) + len(value.Runtime) + len(value.Model) + len(value.Variant) + len(value.Thinking) + len(value.Instructions)
	for key, action := range value.Permissions {
		decodedBytes += len(key) + len(action)
	}
	if decodedBytes > MaxContentSize {
		return Profile{}, ErrProfileTooLarge
	}
	return profileFromConfiguration(value.Path, frontMatter{Name: value.Name, Role: value.Role, Runtime: value.Runtime, Model: value.Model, Variant: value.Variant, Thinking: value.Thinking, Steps: value.Steps, Permissions: value.Permissions}, []byte(value.Instructions), sha256.Sum256(raw), policies)
}
