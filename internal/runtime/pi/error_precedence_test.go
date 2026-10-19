package pi

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
)

type observedWrite struct {
	net.Conn
	started chan struct{}
}

func (c observedWrite) Write(content []byte) (int, error) {
	close(c.started)
	return c.Conn.Write(content)
}

// Run the real reader and writer until a peer failure has closed the connection
// and interrupted a blocked write. Neither completion waiter has started yet.
func failedWrite(t *testing.T, ctx context.Context, record string) (*connection, error) {
	t.Helper()
	client, peer := net.Pipe()
	started := make(chan struct{})
	c := Connect(observedWrite{client, started}, Options{}).(*connection)
	t.Cleanup(func() { _ = peer.Close(); _ = c.Close() })
	job := writeJob{content: []byte("{\"type\":\"get_state\"}\n"), done: make(chan error, 1)}
	select {
	case c.writes <- job:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if record == "" {
		_ = peer.Close()
	} else {
		deadline, _ := ctx.Deadline()
		if err := peer.SetWriteDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.Write([]byte(record + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-job.done:
		if err == nil {
			t.Fatal("blocked write succeeded after connection failure")
		}
		return c, err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil, nil
	}
}

func TestWriteCompletionPreservesRecordedFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		record  string
		failure error
	}{
		{"configuration", `{"type":"omnigrex_configuration_error","error":"secret must not escape"}`, ErrConfiguration},
		{"protocol", "invalid JSON secret must not escape", agent.ErrProtocol},
		{"transport", "", agent.ErrTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, phase := range []string{"bootstrap", "response"} {
				t.Run(phase, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					c, writeErr := failedWrite(t, ctx, tc.record)
					// Replay the observed write completion with c.done already closed.
					// Both failure signals are ready before either waiter starts, so
					// repeated selections exercise both branches without timing sleeps.
					for range 100 {
						written := make(chan error, 1)
						written <- writeErr
						var err error
						if phase == "bootstrap" {
							err = c.awaitBootstrap(ctx, written)
						} else {
							pending := &pendingRequest{result: make(chan response, 1)}
							_, err = c.awaitResponse(ctx, pending, written)
						}
						if !errors.Is(err, tc.failure) {
							t.Fatalf("want %v after failed write, got %v", tc.failure, err)
						}
					}
				})
			}
		})
	}
}
