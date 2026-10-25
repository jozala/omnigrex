package pi_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

// Exercise the owned pre-serialization projection and the actual bounded Go
// reader together. The fixture is synthetic, never an incident transcript.
func TestProjectedLongAgentEndReachesSettlement(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for owned Pi projection test")
	}
	module, err := filepath.Abs("../../../agent/pi/control-events.mjs")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := json.Marshal(module)
	script := `import { controlEvent } from ` + string(path) + `;
 const event = {type:'agent_end', willRetry:false, messages:[{role:'assistant',content:'synthetic'.repeat(400000)}]};
 const output = process.argv[1] === 'original' ? event : controlEvent(event);
 process.stdout.write(JSON.stringify(output)+'\n');`
	original, err := exec.Command(node, "--input-type=module", "-e", script, "original").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(original) < 3<<20 {
		t.Fatal("fixture is not multi-megabyte")
	}
	scanner := bufio.NewScanner(bytes.NewReader(append(original, []byte("{\"type\":\"agent_settled\"}\n")...)))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	if scanner.Scan() || !errors.Is(scanner.Err(), bufio.ErrTooLong) {
		t.Fatalf("old wire did not reproduce framing failure: %v", scanner.Err())
	}
	projected, err := exec.Command(node, "--input-type=module", "-e", script, "projected").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) > 64 || strings.Contains(string(projected), "messages") {
		t.Fatalf("projection is not compact: %d bytes", len(projected))
	}
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
		_, _ = runtime.Write(projected)
		writeRecord(runtime, map[string]any{"type": "agent_settled"})
	})
	outcome, err := c.Prompt(ctx, "long synthetic Turn", nil)
	if err != nil || outcome.Status != agent.Completed {
		t.Fatalf("projected receipt not consumed: %+v %v", outcome, err)
	}
}

func TestOversizedRequiredControlEvidenceStillFailsClosed(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{
			"role": "assistant", "stopReason": "error", "errorMessage": strings.Repeat("private-body", 100000),
		}})
		writeRecord(runtime, map[string]any{"type": "agent_settled"})
	})
	outcome, err := c.Prompt(ctx, "oversized required evidence", nil)
	if !errors.Is(err, agent.ErrProtocol) || outcome.Status != agent.Unknown {
		t.Fatalf("oversized required record did not fail closed: %+v %v", outcome, err)
	}
	if strings.Contains(err.Error(), "private-body") || strings.Contains(outcome.FailureClass, "private-body") {
		t.Fatal("raw required record leaked into diagnostics")
	}
}
