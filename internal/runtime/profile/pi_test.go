package profile_test

import (
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestPiRuntimeProfilePinsSeparateStateAndDisposableInputs(t *testing.T) {
	p, err := profile.NewPiV1("registry.example/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Contract()
	if c.Name != "pi-rpc" || c.Version != "v1" || c.RepositoryEnvironment == nil || c.Workspace != "/workspace" {
		t.Fatalf("contract: %+v", c)
	}
	state, auth := "", ""
	for _, mount := range c.Mounts {
		if mount.Name == "state" {
			state = mount.Path
		}
	}
	for _, tmp := range c.Tmpfs {
		if tmp.Path == "/run/pi-agent" {
			auth = tmp.Path
		}
	}
	if state != "/state" || auth == "" {
		t.Fatal("retained state and credentials are not isolated")
	}
	c.Command = []string{"arbitrary"}
	if _, err := profile.New(c); err == nil {
		t.Fatal("unqualified command accepted")
	}
}
