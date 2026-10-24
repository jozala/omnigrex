package config_test

import (
	"github.com/jozala/omnigrex/internal/config"
	"testing"
)

func TestOperatorStepBudget(t *testing.T) {
	for _, v := range []string{"unlimited", "1", "100", "2147483647"} {
		got, err := config.Load(environment(nil), func(string) (string, bool) { return v, true })
		if err != nil || got.AgentMaxSteps.String() != v {
			t.Fatalf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"", "0", "-1", "1.5", "2147483648", "unknown"} {
		if _, err := config.Load(environment(nil), func(string) (string, bool) { return v, true }); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	got, err := config.Load(environment(nil), func(string) (string, bool) { return "", false })
	if err != nil || got.AgentMaxSteps.Finite() {
		t.Fatal("unset is not unlimited", err)
	}
}
