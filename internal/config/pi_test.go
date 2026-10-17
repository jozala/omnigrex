package config_test

import (
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/config"
)

func TestPiStartupTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  time.Duration
		valid bool
	}{
		{"", 10 * time.Minute, true}, {"45s", 45 * time.Second, true}, {"1us", time.Microsecond, true},
		{"0", 0, false}, {"-1s", 0, false}, {"not-a-duration", 0, false}, {"8761h", 0, false},
	} {
		got, err := config.Load(environment(map[string]string{"OMNIGREX_PI_STARTUP_TIMEOUT": tc.input}))
		if tc.valid && (err != nil || got.PiStartupTimeout != tc.want) || !tc.valid && err == nil {
			t.Errorf("startup timeout %q = %s, %v", tc.input, got.PiStartupTimeout, err)
		}
	}
}

func TestPiConfigurationRequiresCompleteImmutableBinding(t *testing.T) {
	for _, tc := range []struct {
		image, platform string
		valid           bool
	}{
		{"", "", false}, {deploymentImage, "linux/amd64", true}, {deploymentImage, "", false}, {"", "linux/amd64", false}, {"registry.example/pi:latest", "linux/amd64", false}, {deploymentImage, "darwin/arm64", false},
	} {
		got, err := config.Load(environment(map[string]string{"OMNIGREX_PI_IMAGE": tc.image, "OMNIGREX_PI_PLATFORM": tc.platform}))
		if tc.valid {
			if err != nil || got.PiImage != tc.image {
				t.Fatalf("candidate: %+v %v", got, err)
			}
		} else if err == nil {
			t.Fatalf("invalid candidate accepted: %+v", tc)
		}
	}
}

func TestMissingImageSettingsCannotSupplyPiBinding(t *testing.T) {
	_, err := config.Load(environment(map[string]string{
		"OMNIGREX_PI_IMAGE": "", "OMNIGREX_PI_PLATFORM": "",
	}))
	if err == nil {
		t.Fatal("missing image settings supplied an implicit Pi contract")
	}
	if _, err := (config.Config{}).RuntimeProfiles(); err == nil {
		t.Fatal("empty configuration supplied a default Runtime Profile")
	}
}
