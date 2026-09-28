package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/store"
)

func TestInvocationFormattingDoesNotExposeTurnFence(t *testing.T) {
	const secret = "private-turn-owner-token"
	invocation := Invocation{lease: store.AgentTurnLease{OwnerToken: secret}}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprint(invocation), fmt.Sprintf("%#v", invocation), string(encoded)} {
		if strings.Contains(rendered, secret) {
			t.Fatal("formatted MCP invocation exposed its Turn fence")
		}
	}
}
