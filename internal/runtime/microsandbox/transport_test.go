package microsandbox

import (
	"context"
	"errors"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"io"
	"sync"
	"testing"
	"time"
)

func TestTransportClosureInterruptsBlockedReadsWritesAndSlowStderr(t *testing.T) {
	session := &testStream{events: make(chan event, 4), writing: make(chan struct{})}
	session.events <- event{Kind: "stderr", Data: []byte("diagnostic")}
	sink := &blockingSink{started: make(chan struct{}), release: make(chan struct{})}
	defer close(sink.release)
	transport := newTransport()
	if err := transport.attach(session, sink, nil); err != nil {
		t.Fatal(err)
	}
	<-sink.started
	writeDone := make(chan error, 1)
	go func() { _, err := transport.Write([]byte("blocked")); writeDone <- err }()
	<-session.writing
	readDone := make(chan error, 1)
	go func() { _, err := transport.Read(make([]byte, 1)); readDone <- err }()
	start := time.Now()
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("detachment blocked on stream or stderr")
	}
	for _, done := range []chan error{readDone, writeDone} {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("blocked IO fabricated success")
			}
		case <-time.After(time.Second):
			t.Fatal("closed IO remained blocked")
		}
	}
}

func TestTransportSeparatesStderrAndPreservesPositiveExit(t *testing.T) {
	session := &testStream{events: make(chan event, 4), writing: make(chan struct{})}
	session.events <- event{Kind: "stdout", Data: []byte("protocol\n")}
	session.events <- event{Kind: "stderr", Data: []byte("diagnostic\n")}
	session.events <- event{Kind: "exited", ExitCode: 7}
	transport := newTransport()
	exited := make(chan int, 1)
	if err := transport.attach(session, io.Discard, func(code int) { exited <- code }); err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(transport)
	if err != nil || string(content) != "protocol\n" {
		t.Fatalf("stdout=%q,%v", content, err)
	}
	if code := <-exited; code != 7 {
		t.Fatal("lost observed exit code")
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
}

type testStream struct {
	events  chan event
	writing chan struct{}
	once    sync.Once
}

func (stream *testStream) Next(ctx context.Context) (event, error) {
	select {
	case record := <-stream.events:
		return record, nil
	case <-ctx.Done():
		return event{}, ctx.Err()
	}
}
func (stream *testStream) Write(ctx context.Context, _ []byte) (int, error) {
	stream.once.Do(func() { close(stream.writing) })
	<-ctx.Done()
	return 0, ctx.Err()
}
func (*testStream) Close() error { return nil }

type blockingSink struct {
	started, release chan struct{}
	once             sync.Once
}

func (sink *blockingSink) Write(data []byte) (int, error) {
	sink.once.Do(func() { close(sink.started) })
	<-sink.release
	return len(data), nil
}

func TestRetriedSpecificationCannotFollowCallerMutation(t *testing.T) {
	backend, _, ledger := testBackend(t)
	request := testRequest()
	engine, err := backend.New(request, ledger)
	if err != nil {
		t.Fatal(err)
	}
	request.Command[0] = "/bin/false"
	if _, err := engine.Create(context.Background(), request, io.Discard); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("mutated request=%v", err)
	}
}
