package microsandbox

import (
	"context"
	"errors"
	"io"

	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jozala/omnigrex/internal/gitdelivery"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"golang.org/x/sys/unix"
)

func (backend *Backend) ReadWorkspace(ctx context.Context, request execution.CaptureRequest, seal func(io.Reader) error) error {
	return backend.withWorkspace(ctx, request, seal, nil)
}

func (backend *Backend) withWorkspace(ctx context.Context, request execution.CaptureRequest, seal func(io.Reader) error, snapshot *gitdelivery.Snapshot) error {
	if (seal == nil && snapshot == nil) || request.Purpose != execution.PublicationCapture && request.Purpose != execution.TerminalCapture {
		return execution.ErrInvalidSpec
	}
	return backend.locked(ctx, request.Workspace.Key, func(record *operation) (err error) {
		if !record.Created || record.Removed {
			return execution.ErrUnknownEffect
		}
		actual, err := backend.inspect(ctx, nameFor(record.Key))
		if err != nil || !backend.owned(actual, record.Key, record.Digest) || actual.ID != record.ResourceID {
			return errors.Join(execution.ErrConflict, err)
		}
		inventory, err := backend.List(ctx)
		if err != nil {
			return err
		}
		if len(inventory.Malformed) > 0 {
			return execution.ErrUnknownEffect
		}
		found := false
		for _, candidate := range inventory.Processes {
			if candidate.Resource == resourceRef(actual) {
				found = true
			}
		}
		if !found {
			return execution.ErrUnknownEffect
		}
		for _, duplicate := range inventory.Duplicates {
			if duplicate.Identity.AssignmentID == record.Key.AssignmentID {
				return execution.ErrConflict
			}
		}
		for _, candidate := range inventory.Processes {
			if candidate.Identity.AssignmentID == record.Key.AssignmentID && candidate.Resource != resourceRef(actual) {
				other, err := backend.resolveRef(ctx, candidate.Resource)
				if err != nil {
					return err
				}
				if !terminalEnvironmentStatus(other.Status) {
					return execution.ErrConflict
				}
			}
		}
		if request.Purpose == execution.TerminalCapture {
			if !terminalEnvironmentStatus(actual.Status) || !record.Stopped || !record.Flushed {
				return execution.ErrUnknownEffect
			}
		} else {
			if actual.Status != "Running" {
				return execution.ErrConflict
			}
			defer func() {
				resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
				if resumeErr := backend.platform.Resume(resumeCtx, actual); resumeErr != nil {
					err = errors.Join(execution.ErrUnknownEffect, err, resumeErr)
				}
			}()
			if err := backend.platform.Pause(ctx, actual); err != nil {
				return err
			}
		}
		location := backend.location(record.Key, "workspace")
		var stat unix.Stat_t
		if err := unix.Lstat(location, &stat); err != nil {
			return err
		}
		if record.WorkspaceInode == 0 || stat.Ino != record.WorkspaceInode || uint64(stat.Dev) != record.WorkspaceDevice {
			return execution.ErrConflict
		}
		command := exec.CommandContext(ctx, "tar", "-cf", "-", "-C", filepath.Dir(location), filepath.Base(location))
		command.WaitDelay = 2 * time.Second
		command.Stderr = io.Discard
		reader, err := command.StdoutPipe()
		if err != nil {
			return err
		}
		if err := command.Start(); err != nil {
			return err
		}
		var sealErr error
		var files []gitdelivery.File
		if snapshot == nil {
			sealErr = seal(reader)
		} else {
			files, sealErr = gitdelivery.Delta(reader, *snapshot)
		}
		_ = reader.Close()
		if sealErr != nil {
			_ = command.Process.Kill()
		}
		if err := errors.Join(sealErr, command.Wait()); err != nil {
			return err
		}
		if snapshot == nil {
			return nil
		}
		root, err := os.OpenRoot(location)
		if err != nil {
			return err
		}
		defer root.Close()
		writing := false
		defer func() {
			if err != nil && writing {
				err = errors.Join(execution.ErrUnknownEffect, err)
			}
		}()
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			writing = true
			if f.Directory {
				if err := root.Mkdir(f.Name, 0755); err != nil {
					return err
				}
				continue
			}
			if err := root.MkdirAll(filepath.Dir(f.Name), 0755); err != nil {
				return err
			}
			out, err := root.OpenFile(f.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
			if err != nil {
				return err
			}
			_, writeErr := out.Write(f.Content)
			if err := errors.Join(writeErr, out.Sync(), out.Close()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (backend *Backend) InstallSnapshot(ctx context.Context, ref execution.WorkspaceRef, snapshot gitdelivery.Snapshot) error {
	return backend.withWorkspace(ctx, execution.CaptureRequest{Workspace: ref, Purpose: execution.PublicationCapture}, nil, &snapshot)
}
