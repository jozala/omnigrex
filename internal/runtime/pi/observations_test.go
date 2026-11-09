package pi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTerminalToolOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		timing string
		want   *time.Duration
	}{
		{`1.25`, durationPointer(1250 * time.Microsecond)}, {`0`, durationPointer(0)},
		{`null`, nil}, {`-1`, nil}, {`"SENTINEL_SECRET"`, nil}, {`{}`, nil}, {`86400001`, nil}, {`1e100`, nil},
	} {
		got, id := terminalTool([]byte(`{"toolName":"omnigrex_get_issue","toolCallId":"call_1","isError":true,"durationMs":` + tc.timing + `}`))
		if got == nil || got.Layer != "mcp_client" || got.Name != "get_issue" || !got.Error || id != "call_1" {
			t.Fatalf("%s: %+v %q", tc.timing, got, id)
		}
		if (got.Duration == nil) != (tc.want == nil) || got.Duration != nil && *got.Duration != *tc.want {
			t.Fatalf("timing %s: %+v", tc.timing, got)
		}
	}
	for _, name := range []string{"read", "edit", "write", "bash", "grep", "find", "ls"} {
		got, id := terminalTool([]byte(`{"toolName":"` + name + `","toolCallId":"` + strings.Repeat("x", 129) + `","isError":false}`))
		if got == nil || got.Duration != nil || got.Layer != "native" || id != "" {
			t.Fatal(got, id)
		}
	}
	for _, content := range []string{`{"toolName":"SENTINEL_SECRET","isError":false}`, `{"toolName":"omnigrex_unknown","isError":false}`, `{"toolName":"read"}`, `{"toolName":"read","isError":"SENTINEL_SECRET"}`} {
		if got, _ := terminalTool([]byte(content)); got != nil {
			t.Fatalf("untrusted terminal: %+v", got)
		}
	}
}
func durationPointer(d time.Duration) *time.Duration { return &d }

func TestTemporaryFilesystemOptionalDecode(t *testing.T) {
	good := `[{"resource":"tmp","status":"complete","capacity":67108864,"used":4096,"free":67104768,"available":67104768,"inodes":12,"freeInodes":11},{"resource":"run-pi-agent","status":"unavailable","used":55}]`
	got := temporaryFilesystems(json.RawMessage(good))
	if len(got) != 2 || *got[0].Used != 4096 || *got[0].Capacity != 67108864 || got[1].Used != nil {
		t.Fatalf("samples: %+v", got)
	}
	for _, bad := range []string{`null`, `{}`, `[]`, strings.ReplaceAll(good, `"tmp"`, `"SENTINEL_SECRET"`), strings.ReplaceAll(good, `"used":4096`, `"used":4096,"filename":"SENTINEL_SECRET"`), strings.Repeat(" ", 2049)} {
		if got := temporaryFilesystems(json.RawMessage(bad)); got != nil {
			t.Fatal("malformed accepted", got)
		}
	}
	invalid := strings.ReplaceAll(good, `"used":4096`, `"used":-1`)
	if got := temporaryFilesystems(json.RawMessage(invalid)); len(got) != 2 || got[0].Used != nil || got[0].Capacity == nil {
		t.Fatal("invalid optional field discarded other data", got)
	}
}
