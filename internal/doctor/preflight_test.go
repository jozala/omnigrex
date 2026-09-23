package doctor

import (
	"errors"
	"net/http"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

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

func TestValidateReviewerWebhookConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		configuration githubapi.AppWebhookConfig
		requestErr    error
		wantErr       bool
	}{
		{
			name:       "disabled webhook reported as not found",
			requestErr: &githubapi.APIError{StatusCode: http.StatusNotFound, Method: http.MethodGet, Path: "/app/hook/config"},
		},
		{name: "disabled webhook with empty configuration"},
		{name: "configured webhook", configuration: githubapi.AppWebhookConfig{URL: "https://omnigrex.example/webhooks/github"}, wantErr: true},
		{name: "permission failure", requestErr: &githubapi.APIError{StatusCode: http.StatusForbidden}, wantErr: true},
		{name: "non API failure", requestErr: errors.New("connection failed"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateReviewerWebhookConfiguration(test.configuration, test.requestErr)
			if (err != nil) != test.wantErr {
				t.Errorf("validateReviewerWebhookConfiguration() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}
