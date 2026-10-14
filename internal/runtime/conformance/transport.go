//go:build integration

// Package conformance supplies the same observable execution assertions to every backend.
package conformance

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func Transport(t *testing.T, ctx context.Context, factory execution.Factory, request execution.Request, ledger execution.Ledger) {
	t.Helper()
	engine, err := factory.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := engine.EnsureAbsent(cleanup); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		_ = engine.Close()
	})
	stderr := &Output{}
	process, err := engine.Create(ctx, request, stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := process.ObserveExit(ctx); err != nil || got.Exited || got.Running {
		t.Fatalf("unstarted evidence: %+v, %v", got, err)
	}
	_ = process.Transport().Close()
	retryEngine, err := factory.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer retryEngine.Close()
	retry, err := retryEngine.Create(ctx, request, stderr)
	if err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Command = []string{"/bin/sh", "-c", "exit 0"}
	conflicting, err := factory.New(changed, ledger)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conflicting.Create(ctx, changed, io.Discard)
	_ = conflicting.Close()
	if !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("changed request: %v", err)
	}
	if err := retry.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(retry.Transport())
	read := func() string {
		t.Helper()
		type result struct {
			text string
			err  error
		}
		done := make(chan result, 1)
		go func() { text, err := reader.ReadString('\n'); done <- result{text, err} }()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			return got.text
		case <-ctx.Done():
			_ = retry.Transport().Close()
			t.Fatal("transport deadline exceeded")
			return ""
		}
	}
	if got := read(); got != "{\"ready\":true}\n" {
		t.Fatalf("startup framing: %q", got)
	}
	if _, err := retry.Transport().Write([]byte("{\"id\":7}\n")); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "{\"id\":7}\n" {
		t.Fatalf("round trip: %q", got)
	}
	for deadline := time.Now().Add(time.Second); !strings.Contains(stderr.String(), "stderr-marker") && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "stderr-marker") {
		t.Fatal("stderr not delivered separately")
	}
	_ = retry.Transport().Close()
	if got, err := retry.ObserveExit(ctx); err != nil || !got.Running || got.OOMKilled {
		t.Fatalf("detach implied stop: %+v, %v", got, err)
	}
	if err := retry.Stop(ctx, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if got, err := retry.ObserveExit(ctx); err != nil || !got.Exited || got.Running || got.OOMKilled || got.Resource == "" {
		t.Fatalf("stop evidence: %+v, %v", got, err)
	}
	if err := retry.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureAbsent(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := retry.ObserveExit(ctx); err != nil || got.Exited || got.OOMKilled {
		t.Fatalf("missing evidence: %+v, %v", got, err)
	}
	if _, err := engine.Create(ctx, request, io.Discard); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("revoked create: %v", err)
	}
}

type Output struct {
	mutex   sync.Mutex
	content strings.Builder
}

func (output *Output) Write(data []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.content.Write(data)
}
func (output *Output) String() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.content.String()
}
