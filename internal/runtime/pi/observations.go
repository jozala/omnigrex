package pi

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agentevent"
)

var toolID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// Optional measurements cannot turn malformed/missing fields into protocol
// failures. Raw fields isolate them from decoding of critical control evidence.
func terminalTool(content []byte) (*agentevent.ToolObservation, string) {
	var wire struct {
		Name     string          `json:"toolName"`
		ID       string          `json:"toolCallId"`
		Error    *bool           `json:"isError"`
		Duration json.RawMessage `json:"durationMs"`
	}
	if json.Unmarshal(content, &wire) != nil || wire.Error == nil {
		return nil, ""
	}
	layer, name := "native", wire.Name
	if strings.HasPrefix(name, "omnigrex_") {
		layer, name = "mcp_client", strings.TrimPrefix(name, "omnigrex_")
		if !agentevent.KnownMCPToolName(name) {
			return nil, ""
		}
	} else if !agentevent.KnownNativeToolName(name) {
		return nil, ""
	}
	result := &agentevent.ToolObservation{Layer: layer, Name: name, Error: *wire.Error}
	var ms float64
	if len(wire.Duration) > 0 && string(wire.Duration) != "null" && json.Unmarshal(wire.Duration, &ms) == nil && !math.IsNaN(ms) && !math.IsInf(ms, 0) && ms >= 0 && ms <= 86400000 {
		duration := time.Duration(ms * float64(time.Millisecond))
		result.Duration = &duration
	}
	if !toolID.MatchString(wire.ID) {
		wire.ID = ""
	}
	return result, wire.ID
}

func temporaryFilesystems(data json.RawMessage) []agentevent.TemporaryFilesystem {
	if len(data) == 0 || len(data) > 2048 {
		return nil
	}
	var samples []agentevent.TemporaryFilesystem
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&samples) != nil || len(samples) != 2 {
		return nil
	}
	for i := range samples {
		s := &samples[i]
		if s.Resource != []string{"tmp", "run-pi-agent"}[i] || s.Status != "complete" && s.Status != "unavailable" {
			return nil
		}
		for _, field := range []**float64{&s.Capacity, &s.Used, &s.Free, &s.Available, &s.Inodes, &s.FreeInodes} {
			if s.Status == "unavailable" || *field != nil && (**field < 0 || **field > 9007199254740991 || math.IsNaN(**field) || math.IsInf(**field, 0)) {
				*field = nil
			}
		}
	}
	return samples
}
