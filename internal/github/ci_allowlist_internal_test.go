package github

import (
	"strings"
	"testing"
)

func TestValidateLogDownloadURLRequiresLabelBoundary(t *testing.T) {
	const apiBase = "https://api.github.com"
	for _, location := range []string{
		"https://evilgithubusercontent.com/logs",
		"https://evilobjects.githubusercontent.com/logs",
		"https://evilactions.githubusercontent.com/logs",
		"https://notgithubusercontent.com.evil.example/logs",
		"https://githubusercontent.com.evil.example/logs",
	} {
		t.Run(location, func(t *testing.T) {
			if _, err := validateLogDownloadURL(location, apiBase); err == nil {
				t.Fatalf("lookalike destination accepted: %s", location)
			} else if strings.Contains(err.Error(), location) {
				t.Fatalf("location leaked into error: %v", err)
			}
		})
	}
	for _, location := range []string{
		"https://pipelinesghubeus9.blob.core.windows.net/logs",
		"https://example.actions.githubusercontent.com/logs",
		"https://objects.githubusercontent.com/logs",
		"https://results-receiver.actions.githubusercontent.com/logs",
	} {
		t.Run(location, func(t *testing.T) {
			if _, err := validateLogDownloadURL(location, apiBase); err != nil {
				t.Fatalf("approved destination rejected: %s: %v", location, err)
			}
		})
	}
}
