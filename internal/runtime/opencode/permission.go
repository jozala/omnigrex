package opencode

import (
	"encoding/json"

	"github.com/jozala/omnigrex/internal/runtime/acp"
)

func (profile *RenderedProfile) DecidePermission(request acp.PermissionRequest) acp.PermissionDecision {
	if profile == nil {
		return selectPermissionOption(request.Options, false)
	}
	var toolCall struct {
		ID   string `json:"toolCallId"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(request.ToolCall, &toolCall) != nil || toolCall.ID == "" {
		return selectPermissionOption(request.Options, false)
	}

	allowed := false
	switch toolCall.Kind {
	case "execute":
		allowed = profile.allowsAny("bash")
	case "edit", "delete", "move":
		allowed = profile.allowsAny("edit", "patch")
	case "read":
		allowed = profile.allowsAny("read")
	case "fetch":
		allowed = profile.allowsAny("webfetch")
	case "think":
		allowed = profile.allowsAny("task")
	case "search":
		allowed = profile.allowsAny("glob", "grep", "list")
	case "other":
		allowed = profile.allowsAny("websearch", "codesearch", "todoread", "todowrite", "question", "skill")
	}
	return selectPermissionOption(request.Options, allowed)
}

// The exact runtime-owned OpenCode policy filters individual tools before ACP collapses them into broad kinds.
func (profile *RenderedProfile) allowsAny(tools ...string) bool {
	for _, tool := range tools {
		if profile.policy[tool] == PermissionAllow {
			return true
		}
	}
	return false
}

func selectPermissionOption(options []acp.PermissionOption, allowed bool) acp.PermissionDecision {
	preferred := []string{"reject_once", "reject_always"}
	if allowed {
		preferred = []string{"allow_once", "allow_always"}
	}
	for _, kind := range preferred {
		for _, option := range options {
			if option.ID != "" && option.Name != "" && option.Kind == kind {
				return acp.PermissionDecision{OptionID: option.ID}
			}
		}
	}
	return acp.PermissionDecision{}
}
