package config_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestLoadUsesDefaults(t *testing.T) {
	got, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", got.HTTPAddr, ":8080")
	}
	if got.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %s, want %s", got.ShutdownTimeout, 10*time.Second)
	}
}

func TestLoadRejectsInvalidShutdownTimeout(t *testing.T) {
	_, err := config.Load(func(key string) string {
		if key == "OMNIGREX_SHUTDOWN_TIMEOUT" {
			return "eventually"
		}
		return ""
	})
	if err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
}
