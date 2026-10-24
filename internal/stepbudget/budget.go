// Package stepbudget defines configuration and bounded per-Agent-Turn evidence.
package stepbudget

import (
	"bytes"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"strconv"
)

// Max is representable on 32-bit Go and exactly representable in JavaScript.
const Max = 2147483647

var ErrInvalid = errors.New("max_steps must be unlimited or an integer from 1 through 2147483647")

// Limit is a resolved budget. Its zero value means unlimited, never inherit.
// Profile omission is represented separately by a nil *Limit.
type Limit struct{ steps uint32 }

func Parse(value string) (Limit, error) {
	if value == "unlimited" {
		return Limit{}, nil
	}
	if value == "" {
		return Limit{}, ErrInvalid
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return Limit{}, ErrInvalid
		}
	}
	n, err := strconv.ParseUint(value, 10, 31)
	if err != nil || n == 0 {
		return Limit{}, ErrInvalid
	}
	return Limit{uint32(n)}, nil
}
func (l Limit) Finite() bool  { return l.steps != 0 }
func (l Limit) Steps() uint32 { return l.steps }
func (l Limit) String() string {
	if !l.Finite() {
		return "unlimited"
	}
	return strconv.FormatUint(uint64(l.steps), 10)
}
func (l Limit) MarshalJSON() ([]byte, error) {
	if !l.Finite() {
		return []byte(`"unlimited"`), nil
	}
	return []byte(l.String()), nil
}
func (l *Limit) UnmarshalJSON(raw []byte) error {
	value := string(bytes.TrimSpace(raw))
	if value == `"unlimited"` {
		value = "unlimited"
	}
	parsed, err := Parse(value)
	if err == nil {
		*l = parsed
	}
	return err
}
func (l *Limit) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag != "!!int" && (n.Tag != "!!str" || n.Value != "unlimited") {
		return ErrInvalid
	}
	parsed, err := Parse(n.Value)
	if err == nil {
		*l = parsed
	}
	return err
}
func Resolve(profile *Limit, fallback Limit) Limit {
	if profile != nil {
		return *profile
	}
	return fallback
}

// Observation is the runtime's source of truth, not a Work Item completion
// verdict. Used counts admitted conversational requests; Responses counts their
// finalized responses, including errors/cancellation. Complete means the live
// observation reached settlement, not that the required work is complete.
type Observation struct {
	Limit     Limit  `json:"limit"`
	Used      uint64 `json:"used"`
	Responses uint64 `json:"responses"`
	Summary   bool   `json:"summary"`
	Stopped   bool   `json:"stopped"`
	Complete  bool   `json:"complete"`
}

func (o Observation) Valid() bool {
	return o.Used <= 9007199254740991 && o.Responses <= o.Used && (!o.Limit.Finite() || o.Used <= uint64(o.Limit.Steps())) &&
		(o.Summary == (o.Limit.Finite() && o.Used == uint64(o.Limit.Steps()))) && (!o.Stopped || o.Summary && o.Responses == o.Used) && (!o.Complete || o.Responses == o.Used)
}
func Decode(raw []byte) (Observation, error) {
	var fields map[string]json.RawMessage
	if len(raw) > 1024 || json.Unmarshal(raw, &fields) != nil || len(fields) != 6 {
		return Observation{}, ErrInvalid
	}
	for _, key := range []string{"limit", "used", "responses", "summary", "stopped", "complete"} {
		if len(fields[key]) == 0 || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return Observation{}, ErrInvalid
		}
	}
	var o Observation
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&o) != nil || !o.Valid() {
		return Observation{}, ErrInvalid
	}
	return o, nil
}
