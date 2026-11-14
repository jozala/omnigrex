package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

var errNoExecutionEffects = errors.New("execution creation effects conclusively absent")

type noExecutionEffectsError struct{ cause error }

func (err noExecutionEffectsError) Error() string        { return err.cause.Error() }
func (err noExecutionEffectsError) Unwrap() error        { return err.cause }
func (err noExecutionEffectsError) Is(target error) bool { return target == errNoExecutionEffects }

type oneShotAPI interface {
	ContainerCreate(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error)
	ContainerStart(context.Context, string, mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error)
	ContainerWait(context.Context, string, mobyclient.ContainerWaitOptions) mobyclient.ContainerWaitResult
	ContainerRemove(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

func runOneShotContainer(ctx context.Context, api oneShotAPI, options mobyclient.ContainerCreateOptions, operation string, diagnostics ...*environment.QualificationOutput) (runErr error) {
	return runManagedOneShotContainer(ctx, api, options, operation, nil, nil, diagnostics...)
}

// Persist the setup ID before starting or deleting it, so an interrupted or lost
// removal response can be reconciled by immutable ID after orchestrator restart.
func runManagedOneShotContainer(ctx context.Context, api oneShotAPI, options mobyclient.ContainerCreateOptions, operation string,
	bind func(context.Context, string) error, stopped func(context.Context, string) error, diagnostics ...*environment.QualificationOutput) (runErr error) {
	created, err := api.ContainerCreate(ctx, options)
	if err != nil {
		failure := fmt.Errorf("create %s container: %w", operation, err)
		if errdefs.IsNotFound(err) {
			return noExecutionEffectsError{cause: failure}
		}
		return failure
	}
	if bind != nil {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := bind(recordCtx, created.ID)
		cancel()
		if err != nil {
			// Do not delete an ID that could not be recorded. This never-started
			// resource remains discoverable by its deterministic setup name.
			return fmt.Errorf("record %s resource before start: %w", operation, err)
		}
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := api.ContainerRemove(cleanupCtx, created.ID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			runErr = errors.Join(runErr, fmt.Errorf("remove %s container: %w", operation, err))
		} else if stopped != nil {
			if err := stopped(cleanupCtx, created.ID); err != nil {
				runErr = errors.Join(runErr, err)
			} else if runErr != nil {
				runErr = noExecutionEffectsError{cause: runErr}
			}
		} else if runErr != nil {
			// All later operations address this immutable ID. Once removed, even a
			// delayed start cannot resurrect the setup resource.
			runErr = noExecutionEffectsError{cause: runErr}
		}
	}()

	if _, err := api.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s container: %w", operation, err)
	}
	wait := api.ContainerWait(ctx, created.ID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case result := <-wait.Result:
		if result.Error != nil {
			return fmt.Errorf("wait for %s container: %s", operation, result.Error.Message)
		}
		if result.StatusCode != 0 {
			if len(diagnostics) == 1 && diagnostics[0] != nil {
				diagnostics[0].ObserveNonzeroExit()
				collectQualificationLogs(ctx, api, created.ID, diagnostics[0])
			}
			return fmt.Errorf("%s container exited with status %d", operation, result.StatusCode)
		}
		return nil
	case err := <-wait.Error:
		return fmt.Errorf("wait for %s container: %w", operation, err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// This optional interface is used only by golden-resource callers after an
// observed nonzero exit, before exact-ID cleanup. Other one-shot commands never
// read or forward logs.
type qualificationLogAPI interface {
	ContainerLogs(context.Context, string, mobyclient.ContainerLogsOptions) (mobyclient.ContainerLogsResult, error)
}

func collectQualificationLogs(ctx context.Context, api oneShotAPI, id string, output *environment.QualificationOutput) {
	logs, ok := api.(qualificationLogAPI)
	if !ok {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reader, err := logs.ContainerLogs(bounded, id, mobyclient.ContainerLogsOptions{ShowStderr: true})
	if err != nil {
		return
	}
	defer reader.Close()
	// Do not use a demultiplexer that allocates from untrusted frame lengths.
	// Cap both wire bytes and individual frames, with a fixed-size header.
	wire := &io.LimitedReader{R: reader, N: 32 << 10}
	for {
		var header [8]byte
		_, err := io.ReadFull(wire, header[:])
		if err == io.EOF {
			if wire.N == 0 {
				output.Invalidate()
			}
			return
		}
		if err != nil || header[0] != 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			output.Invalidate()
			return
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if size > 16<<10 {
			output.Invalidate()
			return
		}
		if _, err := io.CopyN(output, wire, size); err != nil {
			output.Invalidate()
			return
		}
	}
}
