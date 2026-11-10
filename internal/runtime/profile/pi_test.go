package profile_test

import (
	"encoding/json"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func TestPiRuntimeProfilePinsSeparateStateAndDisposableInputs(t *testing.T) {
	p, err := profile.NewPiV2("registry.example/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Contract()
	if c.Name != "pi-rpc" || c.Version != "v2" || c.RepositoryEnvironment == nil || c.Workspace != "/workspace" {
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

func TestPiHistoricalResourceContractSurvivesCanonicalStoreResolution(t *testing.T) {
	current, err := profile.NewPiV2("registry.example/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	contract := current.Contract()
	var resources piresources.ResourceContract
	if err := json.Unmarshal(contract.GoldenResourceContract, &resources); err != nil {
		t.Fatal(err)
	}
	resources.NodeVersion = "v24.13.0"
	resources.Files[0].SHA256 = strings.Repeat("b", 64)
	contract.GoldenResourceContract, _ = json.Marshal(resources)
	old, err := profile.New(contract)
	if err != nil {
		t.Fatal(err)
	}
	if old.Binding() == current.Binding() {
		t.Fatal("resource inventory is absent from immutable identity")
	}
	// JSONB can reorder object keys; exact canonical resolution must preserve
	// the original approved inventory rather than substituting current defaults.
	encoded, _ := json.Marshal(old.Contract())
	var object map[string]any
	_ = json.Unmarshal(encoded, &object)
	encoded, _ = json.Marshal(object)
	restored, err := profile.DecodeContract(encoded, old.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if restored.Binding() != old.Binding() || string(restored.Contract().GoldenResourceContract) != string(old.Contract().GoldenResourceContract) {
		t.Fatal("historical golden resources changed on restore")
	}
	contract.GoldenResourceContract[0] = 'x'
	if _, err := profile.New(contract); err == nil {
		t.Fatal("malformed golden resource inventory accepted")
	}
}
