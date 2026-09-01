package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const (
	ProtocolVersion = 1
	WorkspacePath   = "/workspace"
)

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

type InitializeResponse struct {
	ProtocolVersion      int               `json:"protocolVersion"`
	AgentCapabilities    AgentCapabilities `json:"agentCapabilities"`
	RawAgentCapabilities json.RawMessage   `json:"-"`
	AgentInfo            *Implementation   `json:"agentInfo,omitempty"`
	AuthMethods          []json.RawMessage `json:"authMethods,omitempty"`
}

func (response *InitializeResponse) UnmarshalJSON(data []byte) error {
	var payload struct {
		ProtocolVersion   int               `json:"protocolVersion"`
		AgentCapabilities json.RawMessage   `json:"agentCapabilities"`
		AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
		AuthMethods       []json.RawMessage `json:"authMethods,omitempty"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	var capabilities AgentCapabilities
	if len(payload.AgentCapabilities) != 0 {
		if err := json.Unmarshal(payload.AgentCapabilities, &capabilities); err != nil {
			return err
		}
	}
	response.ProtocolVersion = payload.ProtocolVersion
	response.AgentCapabilities = capabilities
	response.RawAgentCapabilities = append(response.RawAgentCapabilities[:0], payload.AgentCapabilities...)
	response.AgentInfo = payload.AgentInfo
	response.AuthMethods = payload.AuthMethods
	return nil
}

type AgentCapabilities struct {
	LoadSession         bool                     `json:"loadSession"`
	MCPCapabilities     MCPCapabilities          `json:"mcpCapabilities"`
	PromptCapabilities  PromptCapabilities       `json:"promptCapabilities"`
	SessionCapabilities AgentSessionCapabilities `json:"sessionCapabilities"`
}

type AgentSessionCapabilities struct {
	List                  json.RawMessage `json:"list"`
	Resume                json.RawMessage `json:"resume"`
	AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type PromptCapabilities struct {
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
	Image           bool `json:"image"`
}

func (capabilities AgentCapabilities) SupportsSessionList() bool {
	return capabilityPresent(capabilities.SessionCapabilities.List)
}

func (capabilities AgentCapabilities) SupportsSessionResume() bool {
	return capabilityPresent(capabilities.SessionCapabilities.Resume)
}

func (capabilities AgentCapabilities) SupportsSessionLoad() bool {
	return capabilities.LoadSession
}

func (capabilities AgentCapabilities) SupportsAdditionalDirectories() bool {
	return capabilityPresent(capabilities.SessionCapabilities.AdditionalDirectories)
}

func capabilityPresent(value json.RawMessage) bool {
	return len(value) != 0 && !bytes.Equal(value, []byte("null"))
}

type CreateSessionRequest struct {
	CWD                   string
	MCPServers            []MCPServer
	AdditionalDirectories []string
}

type ContinueSessionRequest struct {
	SessionID             string
	CWD                   string
	MCPServers            []MCPServer
	AdditionalDirectories []string
}

type Session struct {
	ID            string            `json:"sessionId"`
	Modes         json.RawMessage   `json:"modes,omitempty"`
	ConfigOptions []json.RawMessage `json:"configOptions,omitempty"`
}

type SessionInfo struct {
	ID                    string   `json:"sessionId"`
	CWD                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Title                 string   `json:"title,omitempty"`
	UpdatedAt             string   `json:"updatedAt,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type MCPServer struct {
	Type    string
	Name    string
	Command string
	Args    []string
	Env     []EnvironmentEntry
	URL     string
	Headers []EnvironmentEntry
}

func (server MCPServer) MarshalJSON() ([]byte, error) {
	switch server.Type {
	case "":
		return json.Marshal(struct {
			Name    string             `json:"name"`
			Command string             `json:"command"`
			Args    []string           `json:"args"`
			Env     []EnvironmentEntry `json:"env"`
		}{Name: server.Name, Command: server.Command, Args: server.Args, Env: server.Env})
	case "http", "sse":
		return json.Marshal(struct {
			Type    string             `json:"type"`
			Name    string             `json:"name"`
			URL     string             `json:"url"`
			Headers []EnvironmentEntry `json:"headers"`
		}{Type: server.Type, Name: server.Name, URL: server.URL, Headers: server.Headers})
	default:
		return nil, fmt.Errorf("unsupported MCP server type %q", server.Type)
	}
}

type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

func TextContent(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}

type StopReason string

const (
	StopReasonEndTurn         StopReason = "end_turn"
	StopReasonMaxTokens       StopReason = "max_tokens"
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	StopReasonRefusal         StopReason = "refusal"
	StopReasonCancelled       StopReason = "cancelled"
)

type PromptResponse struct {
	StopReason StopReason `json:"stopReason"`
}

type SessionUpdate struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type PermissionDecision struct {
	OptionID string
}
