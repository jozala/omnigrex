package agentprofile

import (
	"encoding/json"
	"fmt"

	"github.com/jozala/omnigrex/internal/role"
	"gopkg.in/yaml.v3"
)

// DecodeSnapshot validates retained canonical inputs through the same Profile
// parser as repository discovery, including presence-sensitive Pi exclusions.
func DecodeSnapshot(raw json.RawMessage, policies role.PolicyCatalog) (Profile, error) {
	var fields map[string]any
	if len(raw) > MaxContentSize || json.Unmarshal(raw, &fields) != nil {
		return Profile{}, ErrInvalidProfile
	}
	path, ok := fields["path"].(string)
	if !ok {
		return Profile{}, ErrInvalidProfile
	}
	instructions, ok := fields["instructions"].(string)
	if !ok {
		return Profile{}, ErrInvalidProfile
	}
	delete(fields, "path")
	delete(fields, "instructions")
	front, err := yaml.Marshal(fields)
	if err != nil {
		return Profile{}, ErrInvalidProfile
	}
	return Parse(path, []byte(fmt.Sprintf("---\n%s---\n%s", front, instructions)), policies)
}
