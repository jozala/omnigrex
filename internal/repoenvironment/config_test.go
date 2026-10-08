package repoenvironment

import (
	"strings"
	"testing"
)

func TestEnvironmentRequiresStrictPinnedImageAndExplicitCapabilities(t *testing.T) {
	valid := "version: 1\nimage: registry.example/repository@sha256:" + strings.Repeat("a", 64) + "\nplatform: linux/amd64\nrequirements: [nested-containers, tool-installation]\n"
	definition, err := Parse([]byte(valid))
	if err != nil || len(definition.Requirements) != 2 {
		t.Fatalf("parse environment: %+v %v", definition, err)
	}
	for _, content := range []string{
		strings.Replace(valid, "version: 1", "version: 2", 1),
		strings.Replace(valid, "registry.example/repository@sha256:"+strings.Repeat("a", 64), "registry.example/repository:latest", 1),
		valid + "host-mounts: [/var/run/docker.sock]\n", valid + "---\nversion: 1\n",
		strings.Replace(valid, "nested-containers", "unknown", 1),
		strings.Replace(valid, "tool-installation", "nested-containers", 1),
		strings.Replace(valid, "linux/amd64", "darwin/arm64", 1),
	} {
		if _, err := Parse([]byte(content)); err == nil {
			t.Fatalf("accepted invalid environment: %s", content)
		}
	}
}
