package agent

// ModelSelection is exact, never a model pattern or fallback preference.
type ModelSelection struct {
	Model    string `json:"model"`
	Thinking string `json:"thinking,omitempty"`
}

// MCPEndpoint is the authenticated per-Turn HTTP authority supplied by the gateway.
// It is ephemeral and must not be persisted with an Agent Session.
type MCPEndpoint struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Headers []Header `json:"headers"`
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (MCPEndpoint) String() string   { return "authenticated MCP endpoint (<redacted>)" }
func (MCPEndpoint) GoString() string { return "agent.MCPEndpoint{<redacted>}" }
