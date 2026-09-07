package doctor

import "testing"

func TestValidateDeveloperWebhookURL(t *testing.T) {
	if err := validateDeveloperWebhookURL("https://omnigrex.example/webhooks/github"); err != nil {
		t.Fatalf("valid webhook URL error = %v", err)
	}
	for _, value := range []string{
		"http://omnigrex.example/webhooks/github",
		"https://omnigrex.example/wrong",
		"https://localhost/webhooks/github",
		"https://127.0.0.1/webhooks/github",
		"https://10.0.0.1/webhooks/github",
		"https://omnigrex.local/webhooks/github",
		"https://omnigrex.example/webhooks/github?secret=value",
	} {
		if err := validateDeveloperWebhookURL(value); err == nil {
			t.Errorf("validateDeveloperWebhookURL(%q) error = nil", value)
		}
	}
}
