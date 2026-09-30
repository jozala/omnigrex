package config_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestToolCacheDefaults(t *testing.T) {
	got, err := config.Load(environment(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.AssignmentToolCacheWarningBytes != 1024<<20 {
		t.Errorf("AssignmentToolCacheWarningBytes = %d, want %d", got.AssignmentToolCacheWarningBytes, 1024<<20)
	}
	if got.AssignmentToolCachePollInterval != 5*time.Minute {
		t.Errorf("AssignmentToolCachePollInterval = %s, want 5m", got.AssignmentToolCachePollInterval)
	}
}

func TestToolCacheEnvironment(t *testing.T) {
	got, err := config.Load(environment(map[string]string{
		"OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB":   "2048",
		"OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL": "10m",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.AssignmentToolCacheWarningBytes != 2048<<20 {
		t.Errorf("AssignmentToolCacheWarningBytes = %d, want %d", got.AssignmentToolCacheWarningBytes, 2048<<20)
	}
	if got.AssignmentToolCachePollInterval != 10*time.Minute {
		t.Errorf("AssignmentToolCachePollInterval = %s, want 10m", got.AssignmentToolCachePollInterval)
	}
}

func TestToolCacheRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "warning zero", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB", value: "0"},
		{name: "warning negative", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB", value: "-1"},
		{name: "warning fraction", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB", value: "1.5"},
		{name: "warning whitespace", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB", value: " 1024"},
		{name: "poll syntax", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL", value: "often"},
		{name: "poll zero", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL", value: "0s"},
		{name: "poll above maximum", key: "OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL", value: "8760h1us"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := config.Load(environment(map[string]string{test.key: test.value})); err == nil {
				t.Fatal("Load() error = nil, want validation error")
			}
		})
	}
}
