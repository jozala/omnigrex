package docker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/opencontainers/image-spec/specs-go/v1"
)

// Golden resource qualification loads the pinned Node CLI, unlike the small
// storage-only helper. Keep both probe paths bounded with the same headroom.
const goldenResourceProbeMemoryBytes = 256 << 20

// CheckRepositoryEnvironment exercises exact runtime resources in a disposable
// diagnostic helper. It attaches no workspace, credentials, or retained state.
func CheckRepositoryEnvironment(ctx context.Context, profile profile.Profile) error {
	contract := profile.Contract()
	var requirements []execution.Requirement
	if contract.RepositoryEnvironment != nil {
		for _, requirement := range contract.RepositoryEnvironment.Requirements {
			requirements = append(requirements, execution.Requirement(requirement))
		}
	}
	if err := (ExecutionFactory{}).CheckRequirements(requirements); err != nil {
		return err
	}
	script, err := environment.QualificationScript(contract.Platform.Arch, contract.Name, contract.GoldenResourceContract)
	if err != nil {
		return err
	}
	api, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return err
	}
	defer api.Close()
	pids := int64(32)
	// Pi's executable/resource check starts Node and loads the pinned CLI.
	// Its measured startup can exceed 64 MiB; allow bounded headroom without
	// borrowing the Agent Turn's much larger runtime budget or enabling swap.
	options := mobyclient.ContainerCreateOptions{
		Name:       "omnigrex-environment-probe-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform:   &v1.Platform{OS: contract.Platform.OS, Architecture: contract.Platform.Arch},
		Config:     &container.Config{Image: contract.Image, User: "10001:10001", Entrypoint: []string{"/bin/sh", "-ec"}, Cmd: []string{script}, Labels: map[string]string{diagnosticMarkerLabel: "true", diagnosticExpiryLabel: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)}},
		HostConfig: &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, Resources: container.Resources{Memory: goldenResourceProbeMemoryBytes, MemorySwap: goldenResourceProbeMemoryBytes, PidsLimit: &pids}},
	}
	if err := runOneShotContainer(ctx, api, options, "repository golden-resource qualification"); err != nil {
		return fmt.Errorf("repository image does not satisfy golden resources: %w", err)
	}
	return nil
}
