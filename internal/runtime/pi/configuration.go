package pi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/workflow"
)

var ErrConfiguration = errors.New("Pi configuration is unavailable or unsupported")

// Configuration contains ephemeral launch inputs, never a durable Profile binding.
// Credentials travel over the process transport, not backend environment metadata.
type Configuration struct {
	Selection        agent.ModelSelection `json:"selection"`
	Tools            []string             `json:"tools"`
	RepositorySkills bool                 `json:"repositorySkills"`
	Instructions     string               `json:"instructions"`
	MCP              agent.MCPEndpoint    `json:"mcp"`
	MCPTools         []string             `json:"mcpTools"`
	Auth             json.RawMessage      `json:"auth"`
}

func (Configuration) String() string   { return "Pi launch configuration (<redacted>)" }
func (Configuration) GoString() string { return "pi.Configuration{<redacted>}" }

// Render translates a trusted Profile snapshot and the running deployment's
// instructions. The checkout supplies skills, never Profile or MCP authority.
func Render(p agentprofile.Profile, policy role.Policy, operator agentinstructions.Operator, stage workflow.StageDefinition, purpose workflow.TurnPurpose, endpoint agent.MCPEndpoint, credentials []byte) (Configuration, error) {
	if p.Runtime() != "pi-rpc/v1" || p.Role() != policy.Role || stage.Role != policy.Role || agentprofile.ValidatePiPermissions(policy, p.Permissions()) != nil {
		return Configuration{}, ErrConfiguration
	}
	provider, _, ok := strings.Cut(p.Model(), "/")
	if !ok {
		return Configuration{}, ErrConfiguration
	}
	// Select just the requested provider. Reject executable key references, OAuth,
	// environment substitutions, and empty API keys without retaining source errors.
	var bundle map[string]json.RawMessage
	if json.Unmarshal(credentials, &bundle) != nil {
		return Configuration{}, ErrConfiguration
	}
	var entry struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal(bundle[provider], &entry) != nil || entry.Type != "api_key" || strings.TrimSpace(entry.Key) == "" || strings.TrimSpace(entry.Key) != entry.Key || strings.HasPrefix(entry.Key, "!") || strings.ContainsAny(entry.Key, "\x00\r\n") || keyReferencesEnvironment(entry.Key) {
		return Configuration{}, ErrConfiguration
	}
	// Pi's auth reader treats dollar signs as template/escape syntax. Encode
	// every accepted literal dollar so reading the file preserves the exact key.
	entry.Key = strings.ReplaceAll(entry.Key, "$", "$$")
	auth, err := json.Marshal(map[string]any{provider: entry})
	if err != nil {
		return Configuration{}, ErrConfiguration
	}
	result := Configuration{Selection: agent.ModelSelection{Model: p.Model(), Thinking: p.Thinking()}, Tools: []string{}, Instructions: operator.Compose(policy, stage, purpose, p.Instructions()), MCP: endpoint, MCPTools: slices.Clone(policy.MCPTools), Auth: auth}
	for _, name := range []string{"read", "edit", "write", "bash", "grep", "find", "ls"} {
		if p.Permission(name) == agentprofile.Allow {
			result.Tools = append(result.Tools, name)
		}
	}
	result.RepositorySkills = p.Permission("skill") == agentprofile.Allow
	if result.RepositorySkills && p.Permission("read") != agentprofile.Allow {
		return Configuration{}, ErrConfiguration
	}
	if result.validate() != nil {
		return Configuration{}, ErrConfiguration
	}
	return result, nil
}

var environmentReferenceName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Match the pinned Pi template reader's reference boundary, without resolving
// any guest environment variable or executing a key reference.
func keyReferencesEnvironment(key string) bool {
	for i := 0; i+1 < len(key); i++ {
		if key[i] != '$' {
			continue
		}
		switch next := key[i+1]; next {
		case '$', '!':
			i++
		case '{':
			if end := strings.IndexByte(key[i+2:], '}'); end >= 0 {
				if environmentReferenceName.MatchString(key[i+2 : i+2+end]) {
					return true
				}
				i += end + 2
			}
		default:
			if next == '_' || next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z' {
				return true
			}
		}
	}
	return false
}

func (configuration Configuration) validate() error {
	u, err := url.Parse(configuration.MCP.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || configuration.MCP.Name != "omnigrex" || len(configuration.MCP.Headers) != 1 || configuration.MCP.Headers[0].Name != "Authorization" || !strings.HasPrefix(configuration.MCP.Headers[0].Value, "Bearer ") || len(configuration.MCP.Headers[0].Value) <= len("Bearer ") || strings.ContainsAny(configuration.MCP.Headers[0].Value, "\x00\r\n") || len(configuration.MCPTools) == 0 || strings.TrimSpace(configuration.Instructions) == "" {
		return ErrConfiguration
	}
	return nil
}

// Start initializes the owned runner over a bounded stdin frame. Its auth file
// and MCP descriptor live only in /run/pi-agent, a disposable tmpfs on both
// backends. The caller still owns environment termination after any failure.
func Start(ctx context.Context, transport io.ReadWriteCloser, configuration Configuration, options Options) (agent.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if configuration.validate() != nil {
		return nil, ErrConfiguration
	}
	content, err := json.Marshal(configuration)
	if err != nil || len(content) > maxRecordBytes {
		return nil, ErrConfiguration
	}
	options.Selection = &configuration.Selection
	options.RequireMCP = true
	c := Connect(transport, options).(*connection)
	job := writeJob{content: append(content, '\n'), done: make(chan error, 1)}
	bounded, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	select {
	case c.writes <- job:
	case <-bounded.Done():
		_ = c.Close()
		return nil, bounded.Err()
	}
	select {
	case err := <-job.done:
		if err == nil {
			return c, nil
		}
		_ = c.Close()
		return nil, agent.ErrTransport
	case <-bounded.Done():
		_ = c.Close()
		return nil, bounded.Err()
	}
}
