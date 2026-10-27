package docker

import (
	"context"
	"errors"
	"io"

	"strconv"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/gitdelivery"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

// WorkspaceSource reads only an exact managed environment. No guest command or
// prompt completion is needed to suspend every container writer and transfer.
type WorkspaceSource struct {
	client *mobyclient.Client
	mutex  sync.Mutex
}

func NewWorkspaceSource() (*WorkspaceSource, error) {
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, err
	}
	return &WorkspaceSource{client: client}, nil
}

func (source *WorkspaceSource) Close() error { return source.client.Close() }

func (source *WorkspaceSource) ReadWorkspace(ctx context.Context, request execution.CaptureRequest, seal func(io.Reader) error) (err error) {
	if err := source.lock(ctx); err != nil {
		return err
	}
	defer source.mutex.Unlock()
	return source.withWorkspace(ctx, request, seal, nil)
}

func (source *WorkspaceSource) withWorkspace(ctx context.Context, request execution.CaptureRequest, seal func(io.Reader) error, snapshot *gitdelivery.Snapshot) (err error) {
	key := request.Workspace.Key
	if !validCanonicalRuntimeUUID(key.AgentTurnID) || key.ExecutionEpoch <= 0 || key.RuntimeProfileName == "" || key.RuntimeProfileVersion == "" {
		return execution.ErrInvalidSpec
	}
	name := "omnigrex-turn-" + key.AgentTurnID + "-epoch-" + strconv.FormatInt(key.ExecutionEpoch, 10)
	inspected, err := source.client.ContainerInspect(ctx, name, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	actual := inspected.Container
	if actual.ID == "" || actual.Config == nil || actual.State == nil || actual.Config.Labels[RuntimeProcessMarkerLabel] != RuntimeProcessMarkerValue ||
		!hasExactRuntimeLabels(actual.Config.Labels, keyLabels(key)) {
		return execution.ErrConflict
	}
	filters := make(mobyclient.Filters).Add("label", RuntimeProcessMarkerLabel+"="+RuntimeProcessMarkerValue)
	for name, value := range keyLabels(key) {
		filters = filters.Add("label", name+"="+value)
	}
	resources, err := source.client.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return err
	}
	mainResources := 0
	for _, resource := range resources.Items {
		if !hasExactRuntimeLabels(resource.Labels, keyLabels(key)) {
			return execution.ErrConflict
		}
		if resource.Labels["io.omnigrex.assignment-subpath-initializer"] == "true" {
			if resource.State == container.StateRunning {
				return execution.ErrConflict
			}
			continue
		}
		mainResources++
		if resource.ID != actual.ID {
			return execution.ErrConflict
		}
	}
	if mainResources != 1 {
		return execution.ErrConflict
	}
	if request.Purpose == execution.TerminalCapture {
		if actual.State.Running || actual.State.Status != container.StateExited && actual.State.Status != container.StateDead {
			return execution.ErrUnknownEffect
		}
	} else {
		if !actual.State.Running || actual.State.Paused {
			return execution.ErrConflict
		}
		// Even an ambiguous pause acknowledgement requires a bounded resume
		// attempt against the immutable ID; otherwise a lost response strands it.
		defer func() {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_, resumeErr := source.client.ContainerUnpause(resumeCtx, actual.ID, mobyclient.ContainerUnpauseOptions{})
			if resumeErr != nil {
				err = errors.Join(execution.ErrUnknownEffect, err, resumeErr)
			}
		}()
		if _, err := source.client.ContainerPause(ctx, actual.ID, mobyclient.ContainerPauseOptions{}); err != nil {
			return err
		}
		paused, err := source.client.ContainerInspect(ctx, actual.ID, mobyclient.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if paused.Container.State == nil || !paused.Container.State.Paused {
			return execution.ErrUnknownEffect
		}
	}
	archive, err := source.client.CopyFromContainer(ctx, actual.ID, mobyclient.CopyFromContainerOptions{SourcePath: "/workspace"})
	if err != nil {
		return err
	}
	if snapshot == nil {
		defer func() { err = errors.Join(err, archive.Content.Close()) }()
		return seal(archive.Content)
	}
	files, validationErr := gitdelivery.Delta(archive.Content, *snapshot)
	closeErr := archive.Content.Close()
	if err := errors.Join(validationErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	payload, err := gitdelivery.Archive(files)
	if err != nil {
		return err
	}
	defer payload.Close()
	_, err = source.client.CopyToContainer(ctx, actual.ID, mobyclient.CopyToContainerOptions{DestinationPath: "/workspace", Content: payload, CopyUIDGID: true})
	if err != nil {
		return errors.Join(execution.ErrUnknownEffect, err)
	}
	return nil
}

func (source *WorkspaceSource) InstallSnapshot(ctx context.Context, ref execution.WorkspaceRef, snapshot gitdelivery.Snapshot) error {
	if err := source.lock(ctx); err != nil {
		return err
	}
	defer source.mutex.Unlock()
	return source.withWorkspace(ctx, execution.CaptureRequest{Workspace: ref, Purpose: execution.PublicationCapture}, nil, &snapshot)
}

func (source *WorkspaceSource) lock(ctx context.Context) error {
	for !source.mutex.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		source.mutex.Unlock()
		return err
	}
	return nil
}
