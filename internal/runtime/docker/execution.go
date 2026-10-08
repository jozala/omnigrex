package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	mobyclient "github.com/moby/moby/client"
)

// ExecutionFactory resolves the transitional Participant-local storage layout.
// It is constructed only by composition; Steps 05-06 replace these bindings.
type ExecutionFactory struct {
	Network            string
	WorkspaceVolume    string
	RuntimeStateVolume string
	MiseVolume         string
}

func (factory ExecutionFactory) New(request execution.Request, ledger execution.Ledger) (execution.Engine, error) {
	policy, spec, err := factory.Compile(request)
	if err != nil {
		return nil, err
	}
	engine, err := NewEngine(EngineOptions{AgentNetwork: factory.Network, RuntimePolicy: policy})
	if err != nil {
		return nil, err
	}
	if ledger == nil {
		_ = engine.Close()
		return nil, errors.New("execution ledger is required")
	}
	return &executionEngine{engine: engine, spec: spec, ledger: ledger, key: request.Key}, nil
}

// Compile translates trusted logical attachments and identity into Docker policy.
func (factory ExecutionFactory) Compile(request execution.Request) (RuntimePolicy, Spec, error) {
	for _, requirement := range request.Requirements {
		if requirement != execution.ProcessIsolation {
			return RuntimePolicy{}, Spec{}, execution.ErrUnsupportedRequirement
		}
	}
	if request.MemoryBytes <= 0 || request.PIDsLimit <= 0 {
		return RuntimePolicy{}, Spec{}, fmt.Errorf("%w: resource budgets must be positive", ErrInvalidSpec)
	}
	key := request.Key
	if request.Workspace != (execution.WorkspaceRef{}) && request.Workspace.Key != key {
		return RuntimePolicy{}, Spec{}, fmt.Errorf("%w: workspace ownership", ErrInvalidSpec)
	}
	if !validCanonicalRuntimeUUID(key.AssignmentID) || !validCanonicalRuntimeUUID(key.AgentSessionID) ||
		!validCanonicalRuntimeUUID(key.AgentTurnID) || key.ExecutionEpoch <= 0 {
		return RuntimePolicy{}, Spec{}, fmt.Errorf("%w: execution key", ErrInvalidSpec)
	}
	labels := keyLabels(key)
	if err := validateExactRuntimeLabels(labels); err != nil {
		return RuntimePolicy{}, Spec{}, err
	}
	if request.WorkflowID != "" {
		labels["io.omnigrex.workflow"] = request.WorkflowID
	}
	labels[executionDigestLabel] = requestDigest(request)
	root := "assignment-" + key.AssignmentID
	sources := map[string]VolumeMount{
		"workspace": {Name: factory.WorkspaceVolume, Subpath: root + "/workspace"},
		"state":     {Name: factory.RuntimeStateVolume, Subpath: root + "/runtime-state"},
		"mise":      {Name: factory.MiseVolume, Subpath: root + "/mise"},
		"tool-data": {Name: factory.MiseVolume, Subpath: root + "/tool-data"},
	}
	var volumes []VolumeMount
	seen := make(map[string]bool)
	for _, attachment := range request.Attachments {
		volume, ok := sources[attachment.Resource]
		if !ok || volume.Name == "" || seen[attachment.Resource] {
			return RuntimePolicy{}, Spec{}, fmt.Errorf("%w: logical attachment", ErrInvalidSpec)
		}
		seen[attachment.Resource] = true
		volume.Target = attachment.Target
		volumes = append(volumes, volume)
	}
	var temporary []TmpfsMount
	for _, directory := range request.TemporaryDirectories {
		temporary = append(temporary, TmpfsMount{Target: directory.Target, SizeBytes: directory.SizeBytes, Executable: directory.Executable})
	}
	environment := make(map[string]string)
	for _, entry := range request.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return RuntimePolicy{}, Spec{}, ErrInvalidSpec
		}
		if _, duplicate := environment[name]; duplicate {
			return RuntimePolicy{}, Spec{}, ErrInvalidSpec
		}
		environment[name] = value
	}
	spec := Spec{
		Name:  "omnigrex-turn-" + key.AgentTurnID + "-epoch-" + strconv.FormatInt(key.ExecutionEpoch, 10),
		Image: request.Image, Platform: Platform{OS: request.Platform.OS, Architecture: request.Platform.Architecture},
		User: request.User, WorkingDir: request.WorkingDir, Command: slices.Clone(request.Command),
		Environment: slices.Clone(request.Environment), Labels: labels, Volumes: volumes, Tmpfs: temporary,
		Network: factory.Network, MemoryBytes: request.MemoryBytes, PIDsLimit: request.PIDsLimit,
		Reattachable: true,
	}
	policy := RuntimePolicy{
		Image: spec.Image, Platform: spec.Platform, User: spec.User, WorkingDir: spec.WorkingDir,
		Command: slices.Clone(spec.Command), Environment: environment, Labels: maps.Clone(labels),
		Volumes: slices.Clone(volumes), Tmpfs: slices.Clone(temporary), Network: spec.Network,
		MemoryBytes: spec.MemoryBytes, PIDsLimit: spec.PIDsLimit,
		Reattachable: true,
	}
	if _, _, err := validateSpec(spec); err != nil {
		return RuntimePolicy{}, Spec{}, err
	}
	return policy, spec, nil
}

const executionDigestLabel = "io.omnigrex.execution-specification"

type executionEngine struct {
	engine *Engine
	spec   Spec
	ledger execution.Ledger
	key    execution.Key
}

func (engine *executionEngine) Create(ctx context.Context, request execution.Request, stderr io.Writer) (execution.Process, error) {
	if requestDigest(request) != engine.spec.Labels[executionDigestLabel] {
		return nil, execution.ErrConflict
	}
	// Read-only validation must not create an unresolved external-effect record.
	if err := engine.engine.validateAPI(ctx); err != nil {
		return nil, err
	}
	reservation, fresh, err := engine.ledger.ReserveExecution(ctx, engine.key, engine.spec.Labels[executionDigestLabel])
	if err != nil {
		return nil, err
	}
	if !fresh {
		if reservation.Phase != execution.Main || reservation.Resource == "" {
			if reservation.CreationSettled {
				return nil, execution.ErrConflict
			}
			return nil, execution.ErrUnknownEffect
		}
		process, err := attachReserved(ctx, engine.engine.client, engine.spec, reservation.Resource, stderr)
		if process == nil {
			return nil, err
		}
		return &executionProcess{Process: process, owner: engine}, err
	}
	// Keep setup termination evidence separate from the main create. A known
	// setup failure must not leave a fictitious unresolved main creation.
	if err := prepareExecutionSubpaths(ctx, engine.engine.client, engine.spec, engine.ledger, engine.key); err != nil {
		return nil, engine.recordCreationFailure(ctx, err)
	}
	if err := engine.ledger.BeginExecutionPhase(ctx, engine.key, execution.Main); err != nil {
		return nil, err
	}
	process, err := create(ctx, engine.engine.client, engine.spec, stderr)
	if process == nil {
		return nil, engine.recordCreationFailure(ctx, err)
	}
	wrapped := &executionProcess{Process: process, owner: engine}
	if bindErr := engine.ledger.BindExecution(ctx, engine.key, process.ID); bindErr != nil {
		return wrapped, errors.Join(execution.ErrUnknownEffect, err, bindErr)
	}
	return wrapped, err
}

func prepareExecutionSubpaths(ctx context.Context, api dockerAPI, spec Spec, ledger execution.Ledger, key execution.Key) error {
	options, err := buildSubpathCreateOptions(spec)
	if err != nil || options.Config == nil {
		return err
	}
	options.Name = spec.Name + "-setup"
	if err := ledger.BeginExecutionPhase(ctx, key, execution.Setup); err != nil {
		return err
	}
	return runManagedOneShotContainer(ctx, api, options, "assignment subpath initializer",
		func(ctx context.Context, id string) error { return ledger.BindSetupResource(ctx, key, id) },
		func(ctx context.Context, id string) error { return ledger.RecordSetupStopped(ctx, key, id) })
}

func (engine *executionEngine) recordCreationFailure(ctx context.Context, failure error) error {
	if errors.Is(failure, errNoExecutionEffects) {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := engine.ledger.RecordNoExecution(recordCtx, engine.key); err != nil {
			return errors.Join(execution.ErrUnknownEffect, failure, err)
		}
		return failure
	}
	return errors.Join(execution.ErrUnknownEffect, failure)
}
func (engine *executionEngine) Close() error { return engine.engine.Close() }
func (engine *executionEngine) EnsureAbsent(ctx context.Context) error {
	cleaner := newExactLabelCleaner(engine.engine.client, func() error { return nil }, ExactLabelCleanerOptions{StopTimeout: 10 * time.Second, Ledger: engine.ledger})
	return cleaner.EnsureKeyAbsent(ctx, engine.key)
}

type executionProcess struct {
	*Process
	owner *executionEngine
}

func (process *executionProcess) Remove(ctx context.Context) error {
	if err := process.owner.EnsureAbsent(ctx); err != nil {
		return err
	}
	return process.Process.Remove(ctx)
}

func keyLabels(key execution.Key) map[string]string {
	labels := map[string]string{
		RuntimeProcessAssignmentLabel: key.AssignmentID, RuntimeProcessSessionLabel: key.AgentSessionID,
		RuntimeProcessTurnLabel: key.AgentTurnID, RuntimeProcessEpochLabel: strconv.FormatInt(key.ExecutionEpoch, 10),
	}
	if key.RuntimeProfileName != "" || key.RuntimeProfileVersion != "" {
		labels[RuntimeProfileIdentityLabel] = key.RuntimeProfileName + "/" + key.RuntimeProfileVersion
	}
	return labels
}

func (cleaner *ExactLabelCleaner) ObserveKeyExit(ctx context.Context, key execution.Key) (execution.ExitObservation, error) {
	return cleaner.ObserveExactExit(ctx, keyLabels(key))
}
func (cleaner *ExactLabelCleaner) EnsureKeyAbsent(ctx context.Context, key execution.Key) error {
	if cleaner.ledger == nil {
		return cleaner.EnsureAbsent(ctx, keyLabels(key))
	}
	reservation, found, err := cleaner.ledger.RevokeExecution(ctx, key)
	if err != nil {
		return err
	}
	factKey := key
	if found && reservation.Key.AgentTurnID != "" {
		factKey = reservation.Key
	}
	name := "omnigrex-turn-" + key.AgentTurnID + "-epoch-" + strconv.FormatInt(key.ExecutionEpoch, 10)
	if found && reservation.Phase == execution.Setup && reservation.SetupResource == "" && !reservation.CreationSettled {
		id, err := cleaner.resolveCreation(ctx, factKey, reservation.Digest, name+"-setup", true)
		if err != nil {
			return err
		}
		if err := cleaner.ledger.BindSetupResource(ctx, factKey, id); err != nil {
			return err
		}
		reservation.SetupResource = id
	}
	if found && reservation.Phase == execution.Main && reservation.Resource == "" && !reservation.CreationSettled {
		id, err := cleaner.resolveCreation(ctx, factKey, reservation.Digest, name, false)
		if err != nil {
			return err
		}
		if err := cleaner.ledger.BindExecution(ctx, factKey, id); err != nil {
			return err
		}
		reservation.Resource = id
	}
	if err := cleaner.EnsureAbsent(ctx, keyLabels(key)); err != nil {
		return errors.Join(execution.ErrUnknownEffect, err)
	}
	if found && reservation.SetupResource != "" {
		if err := cleaner.confirmResourceAbsent(ctx, reservation.SetupResource); err != nil {
			return err
		}
		if err := cleaner.ledger.RecordSetupStopped(ctx, factKey, reservation.SetupResource); err != nil {
			return err
		}
	}
	if found && reservation.Phase == execution.Setup {
		// Revocation serializes against main-phase admission. Main creation never
		// dispatched, and setup is settled by its immutable ID, not by inventory.
		if err := cleaner.ledger.RecordNoExecution(ctx, factKey); err != nil {
			return err
		}
	}
	if found && reservation.Resource != "" {
		return cleaner.confirmResourceAbsent(ctx, reservation.Resource)
	}
	return nil
}

// EnsureKeyStopped revokes further creation for the epoch and confirms that
// every exact-key workload stopped while preserving its workspace for capture.
func (cleaner *ExactLabelCleaner) EnsureKeyStopped(ctx context.Context, key execution.Key) error {
	labels := keyLabels(key)
	if err := validateExactRuntimeLabels(labels); err != nil {
		return err
	}
	if cleaner.ledger != nil {
		reservation, found, err := cleaner.ledger.RevokeExecution(ctx, key)
		if err != nil {
			return err
		}
		if found && !reservation.CreationSettled {
			factKey := reservation.Key
			name := "omnigrex-turn-" + key.AgentTurnID + "-epoch-" + strconv.FormatInt(key.ExecutionEpoch, 10)
			switch reservation.Phase {
			case execution.Setup:
				if reservation.SetupResource == "" {
					id, err := cleaner.resolveCreation(ctx, factKey, reservation.Digest, name+"-setup", true)
					if err != nil {
						return err
					}
					if err := cleaner.ledger.BindSetupResource(ctx, factKey, id); err != nil {
						return err
					}
				}
			case execution.Main:
				if reservation.Resource == "" {
					id, err := cleaner.resolveCreation(ctx, factKey, reservation.Digest, name, false)
					if err != nil {
						return err
					}
					if err := cleaner.ledger.BindExecution(ctx, factKey, id); err != nil {
						return err
					}
				}
			}
		}
	}
	filters := make(mobyclient.Filters)
	for name, value := range labels {
		filters = filters.Add("label", name+"="+value)
	}
	listed, err := cleaner.api.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return errors.Join(execution.ErrUnknownEffect, err)
	}
	for _, candidate := range listed.Items {
		if candidate.ID == "" || !hasExactRuntimeLabels(candidate.Labels, labels) || !runtimeProcessCandidateLabels(candidate.Labels) {
			return execution.ErrConflict
		}
		// Revalidate immutable identity before dispatch, not just the list filter.
		actual, err := cleaner.api.ContainerInspect(ctx, candidate.ID, mobyclient.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if actual.Container.Config == nil || !hasExactRuntimeLabels(actual.Container.Config.Labels, labels) || !runtimeProcessCandidateLabels(actual.Container.Config.Labels) {
			return execution.ErrConflict
		}
		timeout := cleaner.stopTimeoutSeconds
		if _, err := cleaner.api.ContainerStop(ctx, candidate.ID, mobyclient.ContainerStopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) && !errdefs.IsNotModified(err) {
			return errors.Join(execution.ErrUnknownEffect, err)
		}
		stopped, err := cleaner.api.ContainerInspect(ctx, candidate.ID, mobyclient.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil || stopped.Container.State == nil || stopped.Container.State.Running {
			return errors.Join(execution.ErrUnknownEffect, err)
		}
	}
	return nil
}

func (cleaner *ExactLabelCleaner) resolveCreation(ctx context.Context, key execution.Key, digest, name string, setup bool) (string, error) {
	inspected, err := cleaner.api.ContainerInspect(ctx, name, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return "", errors.Join(execution.ErrUnknownEffect, err)
	}
	actual := inspected.Container
	if actual.ID == "" || actual.Config == nil || actual.Config.Labels[RuntimeProcessMarkerLabel] != RuntimeProcessMarkerValue ||
		!hasExactRuntimeLabels(actual.Config.Labels, keyLabels(key)) || actual.Config.Labels[executionDigestLabel] != digest ||
		(actual.Config.Labels["io.omnigrex.assignment-subpath-initializer"] == "true") != setup {
		return "", execution.ErrConflict
	}
	return actual.ID, nil
}

func (cleaner *ExactLabelCleaner) confirmResourceAbsent(ctx context.Context, id string) error {
	_, err := cleaner.api.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if !errdefs.IsNotFound(err) {
		return errors.Join(execution.ErrUnknownEffect, err)
	}
	return nil
}

func requestDigest(request execution.Request) string {
	request.Environment = slices.Clone(request.Environment)
	for index, entry := range request.Environment {
		name, _, _ := strings.Cut(entry, "=")
		if name == "OPENCODE_AUTH_CONTENT" {
			request.Environment[index] = name + "=<ephemeral>"
		}
	}
	content, _ := json.Marshal(request)
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
