package config_test

import (
	"github.com/jozala/omnigrex/internal/config"
	"testing"
)

func TestProviderAuthModeIsExplicitAndDefaultsToAPIKey(t *testing.T) {
	for _, mode := range []string{"", "api_key", "subscription", "auto", " subscription"} {
		got, directory, err := config.LoadProviderAuth(func(name string) string {
			if name == "OMNIGREX_OPENAI_AUTH_MODE" {
				return mode
			}
			return ""
		})
		if mode == "auto" || mode == " subscription" {
			if err == nil {
				t.Fatal("ambiguous auth mode accepted")
			}
			continue
		}
		if err != nil || directory != config.DefaultProviderAuthRoot {
			t.Fatal(err)
		}
		if mode == "" && got != "api_key" || mode != "" && got != mode {
			t.Fatalf("mode = %s", got)
		}
	}
}
