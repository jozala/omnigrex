package workflow_test

import (
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/workflow"
)

func TestHandoffContextBoundsAndIndependentCleanup(t *testing.T) {
	var missing *workflow.HandoffContext
	if missing.Validate() != nil || missing.Map(strings.TrimSpace) != nil {
		t.Fatal("missing context must remain absent")
	}
	c := &workflow.HandoffContext{Explanation: "α\r\n\r\n- action\x00\u202e"}
	safe := c.Map(workflow.CleanHandoffText)
	if safe.Explanation != "α\n\n- action" || c.Explanation == safe.Explanation {
		t.Fatal(safe)
	}
	c.Evidence = strings.Repeat("&", 1001) // JSON escaping is part of the bound.
	if c.Validate() == nil {
		t.Fatal("oversized encoded context accepted")
	}
	c.Evidence = strings.Repeat("a", 5900)
	if c.Validate() != nil {
		t.Fatal("bounded context rejected")
	}
}

func TestHandoffContextExactEncodedBoundary(t *testing.T) {
	// {"explanation":""} consumes 18 bytes; JSON escaping and UTF-8
	// bytes count toward the limit, not merely input character count.
	c := &workflow.HandoffContext{Explanation: strings.Repeat("a", 5982)}
	if c.Validate() != nil {
		t.Fatal("exact 6000-byte value rejected")
	}
	c.Explanation += "a"
	if c.Validate() == nil {
		t.Fatal("6001-byte value accepted")
	}
	c.Explanation = strings.Repeat("α", 2992)
	if c.Validate() == nil {
		t.Fatal("encoded multibyte overflow accepted")
	}
}
