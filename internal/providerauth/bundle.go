package providerauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
)

// SubscriptionBundle overlays installation-selected authentication rather than
// accepting OAuth snapshots or allowing an API key to become a billing fallback.
// Empty input (including /dev/null in Compose) is valid for subscription-only use.
func SubscriptionBundle(path string) (json.RawMessage, error) {
	bundle := map[string]json.RawMessage{}
	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("provider API-key bundle is unavailable")
		}
		defer func() { clear(content) }()
		if len(content) > 1<<20 {
			return nil, errors.New("provider API-key bundle exceeds size limit")
		}
		if len(bytes.TrimSpace(content)) != 0 && (json.Unmarshal(content, &bundle) != nil || bundle == nil) {
			return nil, errors.New("provider API-key bundle must be a JSON object")
		}
	}
	bundle["openai"] = json.RawMessage(`{"type":"controller"}`)
	encoded, err := json.Marshal(bundle)
	for _, entry := range bundle {
		clear(entry)
	}
	return encoded, err
}
