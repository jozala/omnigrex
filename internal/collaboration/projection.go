package collaboration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// MaxProjectionBytes leaves room for the rest of the envelope, its JSON RPC
// escaping and runtime instructions below Pi's 1 MiB record ceiling.
const MaxProjectionBytes = 96 * 1024
const RecordChunkRunes = 4096

type Projection struct {
	RetrievalRequired bool           `json:"retrieval_required"`
	Counts            map[string]int `json:"counts"`
	Instruction       string         `json:"instruction,omitempty"`
}

func (s *State) Records(participant, turn, collection string) []json.RawMessage {
	var records []json.RawMessage
	switch collection {
	case "human_input", "human_input_history":
		for _, in := range s.Inputs {
			if in.ParticipantID != participant || in.DeliveredTo == "" {
				continue
			}
			if collection == "human_input" && in.DeliveredTo != turn {
				continue
			}
			// The batch is not duplicated in the historical projection.
			if collection == "human_input_history" && in.DeliveredTo == turn {
				continue
			}
			// Delivery bookkeeping must not change chunk contents/fingerprints when
			// the final read commits or when a completed read is retried.
			in.DeliveryPending = false
			raw, _ := json.Marshal(in)
			records = append(records, raw)
		}
	case "clarification_history":
		for _, round := range s.Rounds {
			raw, _ := json.Marshal(round)
			records = append(records, raw)
		}
	}
	return records
}

// Project prepares the delivery projection. Omitted batch contents remain pending
// until successful read-ledger coverage. Persist State and projection atomically.
func (s *State) Project(participant, turn string) map[string]json.RawMessage {
	object := map[string]json.RawMessage{}
	projection := Projection{Counts: map[string]int{}}
	for _, name := range []string{"human_input", "human_input_history", "clarification_history"} {
		records := s.Records(participant, turn, name)
		projection.Counts[name] = len(records)
		if records == nil {
			records = []json.RawMessage{}
		}
		object[name], _ = json.Marshal(records)
	}
	raw, _ := json.Marshal(object)
	if len(raw) > MaxProjectionBytes {
		for name := range object {
			object[name] = json.RawMessage(`[]`)
		}
		projection.RetrievalRequired = true
		for i := range s.Inputs {
			in := &s.Inputs[i]
			if in.ParticipantID == participant && in.DeliveredTo == turn {
				in.DeliveryPending = true
			}
		}
		projection.Instruction = "Before interpreting input or handing off, retrieve every human_input record with get_collaboration_record, starting index 0 and following next_offset/next_index. All pending input is frozen for this Turn; there is no collection delay. Retrieve older human_input_history and clarification_history as needed. Concatenate content chunks verbatim with one record_sha256 per record, then parse the JSON; receipt is not interpretation. Handoff remains fenced until every required chunk has a successful scoped read record."
	}
	object["collaboration_projection"], _ = json.Marshal(projection)
	return object
}

type RecordPage struct {
	RecordSHA256 string `json:"record_sha256"`
	Content      string `json:"content"`
	Index        int    `json:"index"`
	Total        int    `json:"total"`
	NextOffset   *int   `json:"next_offset,omitempty"`
	NextIndex    *int   `json:"next_index,omitempty"`
}

// PageRecord chunks the encoded JSON as Unicode text. JSON escaping is included
// in the chunk budget, and UTF-8 characters are never split or replaced.
func PageRecord(record json.RawMessage, total, index, offset int) (RecordPage, error) {
	if total < 0 || index < 0 || index >= total || offset < 0 {
		return RecordPage{}, ErrInvalid
	}
	raw := string(record)
	if !utf8.ValidString(raw) {
		return RecordPage{}, ErrInvalid
	}
	runes := []rune(raw)
	if offset >= len(runes) {
		return RecordPage{}, ErrInvalid
	}
	end := min(len(runes), offset+RecordChunkRunes)
	page := RecordPage{RecordSHA256: fmt.Sprintf("%x", sha256.Sum256(record)), Content: string(runes[offset:end]), Index: index, Total: total}
	if end < len(runes) {
		page.NextOffset = &end
	} else if index+1 < total {
		next := index + 1
		page.NextIndex = &next
	}
	return page, nil
}
