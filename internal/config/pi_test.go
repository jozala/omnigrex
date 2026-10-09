package config_test

import (
	"github.com/jozala/omnigrex/internal/config"
	"testing"
)

func TestCandidatePiConfigurationRequiresCompleteImmutableBinding(t *testing.T) {
	for _, tc := range []struct {
		image, platform string
		valid           bool
	}{
		{"", "", true}, {deploymentImage, "linux/amd64", true}, {deploymentImage, "", false}, {"", "linux/amd64", false}, {"registry.example/pi:latest", "linux/amd64", false}, {deploymentImage, "darwin/arm64", false},
	} {
		got, err := config.Load(environment(map[string]string{"OMNIGREX_PI_RPC_V1_IMAGE": tc.image, "OMNIGREX_PI_RPC_V1_PLATFORM": tc.platform}))
		if tc.valid {
			if err != nil || got.PiRPCV1Image != tc.image {
				t.Fatalf("candidate: %+v %v", got, err)
			}
		} else if err == nil {
			t.Fatalf("invalid candidate accepted: %+v", tc)
		}
	}
}
