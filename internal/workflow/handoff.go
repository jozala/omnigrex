package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// HandoffContext is reported communication, never transition or authorization input.
// Each field has a distinct placement; evidence alone is expandable.
type HandoffContext struct {
	Explanation   string `json:"explanation,omitempty"`
	Intervention  string `json:"intervention,omitempty"`
	CompletedWork string `json:"completed_work,omitempty"`
	Observations  string `json:"observations,omitempty"`
	Uncertainty   string `json:"uncertainty,omitempty"`
	Evidence      string `json:"evidence,omitempty"`
}

// Map returns an independent copy, allowing every field to share redaction policy
// without flattening the paragraphs needed for publication.
func (c *HandoffContext) Map(f func(string) string) *HandoffContext {
	if c == nil {
		return nil
	}
	return &HandoffContext{f(c.Explanation), f(c.Intervention), f(c.CompletedWork), f(c.Observations), f(c.Uncertainty), f(c.Evidence)}
}

// Validate reserves ample room for escaping, diagnostics and owned boilerplate
// below GitHub's comment limit. Bound encoded bytes, not just Unicode runes.
func (c *HandoffContext) Validate() error {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > 6000 {
		return errors.New("handoff context exceeds 6000 encoded bytes")
	}
	return nil
}

func CleanHandoffText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value))
}
