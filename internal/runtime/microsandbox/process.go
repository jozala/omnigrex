package microsandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

type process struct {
	owner     *engine
	stderr    io.Writer
	transport *transport
}

func newProcess(engine *engine, stderr io.Writer) *process {
	if stderr == nil {
		stderr = io.Discard
	}
	return &process{owner: engine, stderr: stderr, transport: newTransport()}
}
func (process *process) Transport() io.ReadWriteCloser { return process.transport }
func (process *process) Start(ctx context.Context) error {
	backend, request := process.owner.backend, process.owner.request
	return backend.locked(ctx, request.Key, func(record *operation) error {
		if record.Revoked || !record.Created || !record.Prepared || record.Started {
			return execution.ErrConflict
		}
		meta, err := backend.inspect(ctx, nameFor(request.Key))
		if err != nil || !backend.owned(meta, request.Key, record.Digest) || meta.ID != record.ResourceID || meta.Status != "Running" {
			return errors.Join(execution.ErrUnknownEffect, err)
		}
		// Persist before dispatch. Recovery cannot adopt or repeat an ambiguous start.
		record.Started = true
		if err := backend.persist(request.Key, record); err != nil {
			return err
		}
		session, err := backend.platform.Stream(ctx, meta, []string{"/bin/sh", "/run/omnigrex/launch"}, request.User)
		if err != nil {
			return errors.Join(execution.ErrUnknownEffect, err)
		}
		return process.transport.attach(session, process.stderr, func(code int) {
			observation, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = backend.locked(observation, request.Key, func(record *operation) error {
				if record.ResourceID != meta.ID {
					return execution.ErrConflict
				}
				record.ProcessExited = true
				record.ProcessExitCode = code
				record.ProcessExitedAt = time.Now()
				return nil
			})
		})
	})
}
func (process *process) ObserveExit(ctx context.Context) (execution.ExitObservation, error) {
	return process.owner.backend.ObserveKeyExit(ctx, process.owner.request.Key)
}
func (process *process) Stop(ctx context.Context, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return process.owner.backend.clean(ctx, process.owner.request.Key, process.owner.ledger, false)
}
func (process *process) Remove(ctx context.Context) error { return process.owner.EnsureAbsent(ctx) }

// Reads use bounded SDK receive cancellation; writes have a fixed maximum wait.
// Closing only releases the stream and its non-owning SDK handle, never the VM.
type transport struct {
	mutex   sync.Mutex
	reader  *io.PipeReader
	writer  *io.PipeWriter
	session stream
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
	ctx     context.Context
}

func newTransport() *transport {
	reader, writer := io.Pipe()
	return &transport{reader: reader, writer: writer}
}
func (transport *transport) attach(session stream, stderr io.Writer, onExit func(int)) error {
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	if transport.closed {
		_ = session.Close()
		return io.ErrClosedPipe
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport.ctx = ctx
	transport.cancel = cancel
	transport.session = session
	transport.done = make(chan struct{})
	stderrQueue := make(chan []byte, 16)
	go func() {
		for data := range stderrQueue {
			_, _ = stderr.Write(data)
		}
	}()
	go func() {
		defer close(transport.done)
		defer close(stderrQueue)
		defer transport.writer.Close()
		for {
			record, err := session.Next(ctx)
			if err != nil {
				_ = transport.writer.CloseWithError(err)
				return
			}
			switch record.Kind {
			case "stdout":
				if _, err := transport.writer.Write(record.Data); err != nil {
					return
				}
			case "stderr": // Bounded independent delivery; a stalled diagnostic sink cannot hold protocol or cleanup.
				if len(record.Data) > 64<<10 {
					_ = transport.writer.CloseWithError(execution.ErrUnknownEffect)
					return
				}
				select {
				case stderrQueue <- bytes.Clone(record.Data):
				case <-ctx.Done():
					return
				default:
					_ = transport.writer.CloseWithError(execution.ErrUnknownEffect)
					return
				}
			case "done":
				return
			case "exited":
				if onExit != nil {
					onExit(record.ExitCode)
				}
				return
			case "failed":
				_ = transport.writer.CloseWithError(execution.ErrUnknownEffect)
				return
			}
		}
	}()
	return nil
}
func (transport *transport) Read(data []byte) (int, error) { return transport.reader.Read(data) }
func (transport *transport) Write(data []byte) (int, error) {
	transport.mutex.Lock()
	session, closed, lifetime := transport.session, transport.closed, transport.ctx
	transport.mutex.Unlock()
	if session == nil || closed {
		return 0, io.ErrClosedPipe
	}
	ctx, cancel := context.WithTimeout(lifetime, 5*time.Second)
	defer cancel()
	return session.Write(ctx, data)
}
func (transport *transport) Close() error {
	transport.mutex.Lock()
	if transport.closed {
		transport.mutex.Unlock()
		return nil
	}
	transport.closed = true
	cancel, done, session := transport.cancel, transport.done, transport.session
	transport.mutex.Unlock()
	_ = transport.reader.Close()
	_ = transport.writer.Close()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			return execution.ErrUnknownEffect
		}
	}
	if session != nil {
		closed := make(chan error, 1)
		go func() { closed <- session.Close() }()
		select {
		case err := <-closed:
			return err
		case <-time.After(5 * time.Second):
			return execution.ErrUnknownEffect
		}
	}
	return nil
}
