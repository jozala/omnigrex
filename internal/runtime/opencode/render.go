package opencode

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Role string

const (
	RoleDeveloper Role = "developer"
	RoleReviewer  Role = "reviewer"
)

type Permission string

const (
	PermissionAllow Permission = "allow"
	PermissionDeny  Permission = "deny"
)

var ErrInvalidProfile = errors.New("invalid OpenCode profile")

type PermissionPolicy map[string]Permission

type Profile struct {
	Instructions string
	Model        string
	Variant      string
	Steps        uint
	Permissions  PermissionPolicy
}

type RenderedProfile struct {
	role    Role
	config  []byte
	environ []string
	policy  PermissionPolicy
	session SessionConfiguration
}

func Render(role Role, profile Profile) (*RenderedProfile, error) {
	if (role != RoleDeveloper && role != RoleReviewer) ||
		strings.TrimSpace(profile.Instructions) == "" ||
		!utf8.ValidString(profile.Instructions) ||
		!validReference(profile.Model) ||
		(profile.Variant != "" && containsWhitespaceOrControl(profile.Variant)) ||
		profile.Steps == 0 || profile.Steps > 1000 {
		return nil, ErrInvalidProfile
	}
	if role == RoleReviewer && (profile.Permissions["edit"] == PermissionAllow || profile.Permissions["patch"] == PermissionAllow) {
		return nil, ErrInvalidProfile
	}
	if edit, hasEdit := profile.Permissions["edit"]; hasEdit {
		if patch, hasPatch := profile.Permissions["patch"]; hasPatch && patch != edit {
			return nil, ErrInvalidProfile
		}
	}

	permission := map[string]string{"*": "deny"}
	policy := make(PermissionPolicy, len(profile.Permissions))
	for name, action := range profile.Permissions {
		if !knownPermission(name) || (action != PermissionAllow && action != PermissionDeny) {
			return nil, ErrInvalidProfile
		}
		if name == "*" {
			continue
		}
		policy[name] = action
		openCodeName := name
		if name == "patch" {
			openCodeName = "edit"
		}
		if action == PermissionAllow {
			permission[openCodeName] = "ask"
		} else {
			permission[openCodeName] = "deny"
		}
	}

	agentID := "omnigrex-" + string(role)
	type agentConfig struct {
		Mode       string            `json:"mode,omitempty"`
		Permission map[string]string `json:"permission,omitempty"`
		Prompt     string            `json:"prompt,omitempty"`
		Steps      uint              `json:"steps,omitempty"`
		Disable    bool              `json:"disable,omitempty"`
	}
	payload := struct {
		Agent        map[string]agentConfig `json:"agent"`
		Autoupdate   bool                   `json:"autoupdate"`
		DefaultAgent string                 `json:"default_agent"`
		MCP          map[string]any         `json:"mcp"`
		Permission   map[string]string      `json:"permission"`
		Share        string                 `json:"share"`
	}{
		Agent: map[string]agentConfig{
			"build": {Disable: true},
			agentID: {
				Mode:       "primary",
				Permission: permission,
				Prompt:     profile.Instructions,
				Steps:      profile.Steps,
			},
			"plan": {Disable: true},
		},
		Autoupdate:   false,
		DefaultAgent: agentID,
		MCP:          map[string]any{},
		Permission:   permission,
		Share:        "disabled",
	}
	config, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidProfile
	}

	environ := []string{
		"OPENCODE_AUTH_CONTENT={}",
		"OPENCODE_AUTO_SHARE=false",
		"OPENCODE_CONFIG_CONTENT=" + string(config),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	}
	if role == RoleReviewer {
		environ = append(environ,
			"OPENCODE_DISABLE_CLAUDE_CODE=true",
			"OPENCODE_DISABLE_DEFAULT_PLUGINS=true",
			"OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
			"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		)
	}
	environ = append(environ, "OPENCODE_DISABLE_SHARE=1")
	if role == RoleReviewer {
		environ = append(environ, "OPENCODE_PURE=true")
	}

	return &RenderedProfile{
		role:    role,
		config:  config,
		environ: environ,
		policy:  policy,
		session: SessionConfiguration{Model: profile.Model, Variant: profile.Variant, Mode: agentID},
	}, nil
}

func (profile *RenderedProfile) ConfigJSON() []byte {
	return append([]byte(nil), profile.config...)
}

func (profile *RenderedProfile) Environment() []string {
	return append([]string(nil), profile.environ...)
}

func (profile *RenderedProfile) SessionConfiguration() SessionConfiguration {
	return profile.session
}

type SessionConfiguration struct {
	Model   string
	Variant string
	Mode    string
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

func knownPermission(name string) bool {
	switch name {
	case "*", "read", "edit", "glob", "grep", "list", "patch", "bash", "task", "webfetch", "websearch",
		"codesearch", "todoread", "todowrite", "question", "skill":
		return true
	default:
		return false
	}
}
