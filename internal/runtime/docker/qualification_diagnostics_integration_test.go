//go:build integration

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func TestDockerNativeQualificationCapturesOnlyFailedResourceCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	api, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	var inventory piresources.ResourceContract
	_ = json.Unmarshal(piresources.CandidateContract("amd64"), &inventory)
	for i := range inventory.Files {
		if inventory.Files[i].Name == "runner.mjs" {
			inventory.Files[i].SHA256 = strings.Repeat("a", 64)
		}
	}
	resources, _ := json.Marshal(inventory)
	script, err := environment.QualificationScript("amd64", "pi-rpc", resources)
	if err != nil {
		t.Fatal(err)
	}
	output := environment.NewQualificationOutput("amd64", "pi-rpc", resources)
	options := mobyclient.ContainerCreateOptions{
		Config:     &container.Config{Image: "omnigrex/pi:1.4.0", User: "10001:10001", Entrypoint: []string{"/bin/sh", "-ec"}, Cmd: []string{"printf 'fixture-secret\\n' >&2\n" + script}},
		HostConfig: &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, Resources: container.Resources{Memory: 256 << 20, MemorySwap: 256 << 20}},
	}
	err = runOneShotContainer(ctx, api, options, "golden qualification", output)
	if err == nil || !strings.Contains(err.Error(), "exited with status") || strings.Contains(err.Error(), "fixture-secret") || !strings.Contains(output.Check(), "owned checksum runner.mjs (guest-reported failed check)") {
		t.Fatalf("native Docker capture: %v; %s", err, output.Check())
	}
	if !errors.Is(err, errNoExecutionEffects) {
		t.Fatal("exact-owned helper cleanup not established", err)
	}
}
