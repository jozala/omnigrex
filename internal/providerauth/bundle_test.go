package providerauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubscriptionBundleOverlaysOpenAIWithoutDeliveringItsAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"type":"api_key","key":"sk-never-fallback"},"other":{"type":"api_key","key":"other-key"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	content, err := SubscriptionBundle(path)
	if err != nil || strings.Contains(string(content), "sk-never-fallback") || !strings.Contains(string(content), "other-key") {
		t.Fatal("subscription mode retained API fallback or lost other providers")
	}
	for _, path := range []string{"", "/dev/null"} {
		content, err := SubscriptionBundle(path)
		var bundle map[string]json.RawMessage
		if err != nil || json.Unmarshal(content, &bundle) != nil || string(bundle["openai"]) != `{"type":"controller"}` {
			t.Fatal("subscription-only installation requires a placeholder API key")
		}
	}
}
