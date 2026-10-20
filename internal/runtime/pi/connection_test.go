package pi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

func TestPromptAdmissionDoesNotSettleTurn(t *testing.T) {
	client, runtime := net.Pipe()
	connection := pi.Connect(client, pi.Options{})
	t.Cleanup(func() { _ = connection.Close(); _ = runtime.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	admitted, finish := make(chan struct{}), make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(runtime)
		for scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			if command.Type == "get_state" {
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "diagnostic", "isStreaming": false}})
				continue
			}
			writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"disposition": "started"}})
			writeRecord(runtime, map[string]any{"type": "agent_end"})
			close(admitted)
			<-finish
			writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": "complete\u2028unicode\u2029safe"}}}})
			writeRecord(runtime, map[string]any{"type": "agent_settled"})
		}
	}()
	if _, err := connection.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan agent.Outcome, 1)
	go func() { outcome, _ := connection.Prompt(ctx, "do work", agentevent.NoopSink{}); done <- outcome }()
	<-admitted
	select {
	case got := <-done:
		t.Fatalf("admission or agent_end settled the Turn: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	select {
	case got := <-done:
		if !got.Admitted || got.Status != agent.Completed {
			t.Fatalf("settled outcome: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func writeRecord(runtime net.Conn, record map[string]any) {
	content, _ := json.Marshal(record)
	_, _ = runtime.Write(append(content, '\n'))
}

func fixture(t *testing.T, options pi.Options, prompt func(net.Conn, string), abort ...func(net.Conn, string)) (agent.Connection, context.Context) {
	t.Helper()
	client, runtime := net.Pipe()
	c := pi.Connect(client, options)
	t.Cleanup(func() { _ = runtime.Close(); _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	go func() {
		scanner := bufio.NewScanner(runtime)
		for scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			switch command.Type {
			case "get_state":
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "diagnostic", "isStreaming": false}})
			case "prompt":
				prompt(runtime, command.ID)
			case "abort":
				if len(abort) > 0 {
					abort[0](runtime, command.ID)
					continue
				}
				settle(runtime, "aborted")
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true})
			}
		}
	}()
	if _, err := c.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	return c, ctx
}

func admit(runtime net.Conn, id string) {
	writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "prompt", "success": true, "data": map[string]any{"disposition": "started"}})
}

func settle(runtime net.Conn, reason string) {
	writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": reason, "errorMessage": "Bearer secret-provider-key"}})
	writeRecord(runtime, map[string]any{"type": "agent_settled"})
}

func TestCallerDeadlinePolicyRejectsUnboundedPromptAndUsesCallerBudget(t *testing.T) {
	c, ctx := fixture(t, pi.Options{CallerDeadlineOnly: true, PromptTimeout: time.Millisecond}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		time.Sleep(20 * time.Millisecond)
		settle(runtime, "stop")
	})
	if outcome, err := c.Prompt(context.Background(), "unbounded", nil); !errors.Is(err, agent.ErrDeadlineRequired) || outcome.Status != agent.Rejected || outcome.Admitted {
		t.Fatalf("unbounded prompt: %+v %v", outcome, err)
	}
	if outcome, err := c.Prompt(ctx, "bounded", nil); err != nil || outcome.Status != agent.Completed {
		t.Fatalf("caller budget was shortened: %+v %v", outcome, err)
	}
}

func TestTerminalOutcomesAndEOF(t *testing.T) {
	for _, tc := range []struct {
		reason string
		status agent.Status
	}{
		{"stop", agent.Completed}, {"length", agent.Completed}, {"error", agent.Failed}, {"aborted", agent.Aborted},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) { admit(runtime, id); settle(runtime, tc.reason); _ = runtime.Close() })
			got, err := c.Prompt(ctx, "work", agentevent.NoopSink{})
			if err != nil || got.Status != tc.status || !got.Admitted {
				t.Fatalf("outcome = %+v, %v", got, err)
			}
		})
	}
}

func TestSubscriptionFailuresRetainOnlyRecognizedClassification(t *testing.T) {
	for _, failure := range []string{"subscription_quota", "subscription_auth", "subscription_configuration", "subscription_temporary", "secret-provider-text"} {
		t.Run(failure, func(t *testing.T) {
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				admit(runtime, id)
				writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "Omnigrex " + failure, "omnigrexProviderGeneration": "00000000-0000-4000-8000-000000000001"}})
				writeRecord(runtime, map[string]any{"type": "agent_settled"})
			})
			outcome, err := c.Prompt(ctx, "work", nil)
			if err != nil || !outcome.Admitted || outcome.Status != agent.Failed {
				t.Fatalf("failed outcome = %+v, %v", outcome, err)
			}
			want := failure
			if failure == "secret-provider-text" {
				want = "provider_failure"
			}
			if outcome.FailureClass != want {
				t.Fatalf("failure = %s", outcome.FailureClass)
			}
		})
	}
}

func TestUnknownOutcomesNeverBecomeCompletion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		action  func(net.Conn)
		failure error
	}{
		{"process loss", func(runtime net.Conn) { _ = runtime.Close() }, agent.ErrTransport},
		{"malformed JSON", func(runtime net.Conn) { _, _ = runtime.Write([]byte("secret-provider-key\n")) }, agent.ErrProtocol},
		{"missing assistant", func(runtime net.Conn) { writeRecord(runtime, map[string]any{"type": "agent_settled"}) }, agent.ErrProtocol},
		{"lost terminal", func(runtime net.Conn) { writeRecord(runtime, map[string]any{"type": "agent_end"}) }, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := fixture(t, pi.Options{PromptTimeout: 50 * time.Millisecond, CancellationTimeout: 50 * time.Millisecond}, func(runtime net.Conn, id string) { admit(runtime, id); tc.action(runtime) })
			got, err := c.Prompt(ctx, "work", agentevent.NoopSink{})
			if !errors.Is(err, tc.failure) || got.Status != agent.Unknown || !got.Admitted {
				t.Fatalf("outcome = %+v, %v", got, err)
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", got, err), "secret-provider-key") {
				t.Fatal("raw failure leaked")
			}
			if next, err := c.Prompt(ctx, "overlap", nil); err == nil || next.Status != agent.Rejected {
				t.Fatal("uncertain transport was reused")
			}
		})
	}
}

func TestCancellationRequiresSettlementAndRejectsConcurrentPrompt(t *testing.T) {
	started := make(chan struct{})
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) { admit(runtime, id); close(started) })
	done := make(chan agent.Outcome, 1)
	go func() { got, _ := c.Prompt(ctx, "hold", nil); done <- got }()
	<-started
	if _, err := c.Prompt(ctx, "overlap", nil); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("overlap: %v", err)
	}
	if err := c.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.Status != agent.Aborted {
			t.Fatalf("abort = %+v", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestContinuousReaderCorrelatesOutOfOrderResponses(t *testing.T) {
	client, runtime := net.Pipe()
	c := pi.Connect(client, pi.Options{})
	t.Cleanup(func() { _ = runtime.Close(); _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		scanner := bufio.NewScanner(runtime)
		var ids []string
		for scanner.Scan() {
			var command struct{ ID string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			ids = append(ids, command.ID)
			if len(ids) == 8 {
				for i := len(ids) - 1; i >= 0; i-- {
					writeRecord(runtime, map[string]any{"type": "response", "id": ids[i], "command": "get_state", "success": true, "data": map[string]any{"sessionId": "diagnostic", "isStreaming": false}})
				}
				return
			}
		}
	}()
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := c.Prepare(ctx); err != nil {
				t.Errorf("prepare: %v", err)
			}
		})
	}
	group.Wait()
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
}

func (s blockingSink) Emit(context.Context, agentevent.AgentEvent) error {
	close(s.entered)
	<-s.release
	return nil
}

func TestSlowConsumerCannotBlockDeadlineOrCleanup(t *testing.T) {
	c, ctx := fixture(t, pi.Options{PromptTimeout: 50 * time.Millisecond, CancellationTimeout: 50 * time.Millisecond}, func(runtime net.Conn, id string) { admit(runtime, id) })
	sink := blockingSink{make(chan struct{}), make(chan struct{})}
	defer close(sink.release)
	start := time.Now()
	got, err := c.Prompt(ctx, "work", sink)
	if got.Status != agent.Unknown || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("slow sink = %+v, %v", got, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEventOverflowFailsExplicitlyWithoutWaitingForSlowSink(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		for range 1000 {
			writeRecord(runtime, map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"delta": "credential-sentinel"}})
		}
	})
	sink := blockingSink{make(chan struct{}), make(chan struct{})}
	defer close(sink.release)
	start := time.Now()
	got, err := c.Prompt(ctx, "work", sink)
	if got.Status != agent.Unknown || !errors.Is(err, agent.ErrBackpressure) || time.Since(start) > time.Second {
		t.Fatalf("overflow = %+v, %v", got, err)
	}
}

func TestBlockedWriteHonorsDeadlineAndClosesTransport(t *testing.T) {
	client, runtime := net.Pipe()
	c := pi.Connect(client, pi.Options{StartupTimeout: 30 * time.Millisecond})
	t.Cleanup(func() { _ = runtime.Close(); _ = c.Close() })
	start := time.Now()
	_, err := c.Prepare(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("blocked write: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadyConnectionUsesOrdinaryBudgetForRepeatedPreparation(t *testing.T) {
	client, runtime := net.Pipe()
	c := pi.Connect(client, pi.Options{StartupTimeout: 300 * time.Millisecond, RequestTimeout: 30 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
	go func() {
		scanner := bufio.NewScanner(runtime)
		count := 0
		for scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			count++
			if count == 2 {
				time.Sleep(90 * time.Millisecond)
			}
			writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "fixture", "isStreaming": false}})
		}
	}()
	if _, err := c.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prepare(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated preparation used startup allowance: %v", err)
	}
}

func TestConfigurationAfterInitialStateUsesOrdinaryRequestBudget(t *testing.T) {
	client, runtime := net.Pipe()
	c := pi.Connect(client, pi.Options{StartupTimeout: 300 * time.Millisecond, RequestTimeout: 30 * time.Millisecond, RequireMCP: true})
	t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
	go func() {
		scanner := bufio.NewScanner(runtime)
		for scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			if command.Type == "get_state" {
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "fixture", "isStreaming": false}})
			} else {
				time.Sleep(90 * time.Millisecond)
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"commands": []any{map[string]string{"name": "omnigrex-ready", "source": "extension"}}}})
			}
		}
	}()
	if _, err := c.Prepare(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("post-readiness RPC used startup allowance: %v", err)
	}
	if outcome, err := c.Prompt(t.Context(), "must not admit", nil); err == nil || outcome.Admitted {
		t.Fatalf("prompt admitted after configuration timeout: %+v %v", outcome, err)
	}
}

func TestStartupRPCRejectionIsNotLoggedAsTransportFailure(t *testing.T) {
	client, runtime := net.Pipe()
	var output bytes.Buffer
	c := pi.Connect(client, pi.Options{Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
	go func() {
		scanner := bufio.NewScanner(runtime)
		if scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": false, "error": "private guest text"})
		}
	}()
	if _, err := c.Prepare(t.Context()); !errors.Is(err, agent.ErrRuntime) {
		t.Fatalf("RPC rejection: %v", err)
	}
	if !strings.Contains(output.String(), `"outcome":"runtime_rejection"`) || strings.Contains(output.String(), "private guest text") {
		t.Fatalf("misclassified or unsafe startup observation: %s", output.String())
	}
}

func TestStartupObservationWaitsForDurablePreparation(t *testing.T) {
	client, runtime := net.Pipe()
	var output bytes.Buffer
	c := pi.Connect(client, pi.Options{DeferStartupCompletion: true, Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
	go func() {
		scanner := bufio.NewScanner(runtime)
		if scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "fixture", "isStreaming": false}})
		}
	}()
	if _, err := c.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "Pi startup readiness completed") {
		t.Fatal("startup logged ready before durable binding")
	}
	completion := c.(interface{ CompletePreparation(error) error })
	if err := completion.CompletePreparation(agent.ErrSession); !errors.Is(err, agent.ErrSession) {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"outcome":"configuration_failure"`) || strings.Contains(output.String(), `"outcome":"ready"`) {
		t.Fatalf("durable binding failure logged as ready: %s", output.String())
	}
}

func TestStartupObservationDoesNotCallNonTransportFailuresTransportLoss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
	}{
		{"busy", agent.ErrBusy, "preparation_failure"},
		{"binding", errors.New("durable binding failed"), "preparation_failure"},
		{"transport", agent.ErrTransport, "transport_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, runtime := net.Pipe()
			var output bytes.Buffer
			c := pi.Connect(client, pi.Options{Logger: slog.New(slog.NewJSONHandler(&output, nil))})
			t.Cleanup(func() { _ = c.Close(); _ = runtime.Close() })
			c.(interface{ CompletePreparation(error) error }).CompletePreparation(tc.err)
			if !strings.Contains(output.String(), `"outcome":"`+tc.outcome+`"`) {
				t.Fatalf("misclassified %s: %s", tc.name, output.String())
			}
		})
	}
}

type slowClose struct {
	net.Conn
	release chan struct{}
}

func (c slowClose) Close() error { <-c.release; return c.Conn.Close() }

func TestBlockedTransportCloseIsBounded(t *testing.T) {
	client, runtime := net.Pipe()
	blocked := slowClose{client, make(chan struct{})}
	t.Cleanup(func() { close(blocked.release); _ = runtime.Close() })
	c := pi.Connect(blocked, pi.Options{CloseTimeout: 30 * time.Millisecond})
	start := time.Now()
	if err := c.Close(); !errors.Is(err, agent.ErrCleanup) || time.Since(start) > time.Second {
		t.Fatalf("blocked close: %v", err)
	}
}

func TestMismatchedResponseFailsWithoutLeakingSourceError(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": false, "error": "credential-sentinel"})
	})
	got, err := c.Prompt(ctx, "work", nil)
	if !errors.Is(err, agent.ErrProtocol) || got.Status != agent.Unknown || strings.Contains(fmt.Sprintf("%+v %#v", got, err), "credential-sentinel") {
		t.Fatalf("mismatched response: %+v, %v", got, err)
	}
}

type delayedPromptWrite struct {
	net.Conn
	release chan struct{}
}

func (c delayedPromptWrite) Write(content []byte) (int, error) {
	n, err := c.Conn.Write(content)
	if strings.Contains(string(content), `"type":"prompt"`) {
		<-c.release
	}
	return n, err
}

func TestSettlementBeforeWriteAcknowledgementSurvivesEOF(t *testing.T) {
	client, runtime := net.Pipe()
	release := make(chan struct{})
	c := pi.Connect(delayedPromptWrite{client, release}, pi.Options{})
	t.Cleanup(func() { close(release); _ = runtime.Close(); _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		scanner := bufio.NewScanner(runtime)
		for scanner.Scan() {
			var command struct{ ID, Type string }
			_ = json.Unmarshal(scanner.Bytes(), &command)
			if command.Type == "get_state" {
				writeRecord(runtime, map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": true, "data": map[string]any{"sessionId": "diagnostic", "isStreaming": false}})
				continue
			}
			admit(runtime, command.ID)
			settle(runtime, "stop")
			_ = runtime.Close()
			return
		}
	}()
	if _, err := c.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.Prompt(ctx, "work", nil)
	if err != nil || got.Status != agent.Completed || !got.Admitted {
		t.Fatalf("buffered settlement = %+v, %v", got, err)
	}
}

func TestMalformedAdmissionDoesNotClaimStartedWork(t *testing.T) {
	for _, data := range []any{nil, map[string]any{}, map[string]any{"disposition": "queued"}} {
		t.Run(fmt.Sprint(data), func(t *testing.T) {
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "prompt", "success": true, "data": data})
			})
			got, err := c.Prompt(ctx, "work", nil)
			if got.Admitted || got.Status != agent.Unknown || !errors.Is(err, agent.ErrProtocol) {
				t.Fatalf("invalid admission = %+v, %v", got, err)
			}
		})
	}
}

func TestCommandRejectionAndRuntimeLossRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record map[string]any
		status agent.Status
	}{
		{"command rejection", map[string]any{"type": "response", "command": "prompt", "success": false, "error": "credential-sentinel"}, agent.Rejected},
		{"runtime failure", map[string]any{"type": "extension_error", "error": "credential-sentinel"}, agent.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				record := tc.record
				record["id"] = id
				writeRecord(runtime, record)
			})
			got, err := c.Prompt(ctx, "work", nil)
			if got.Status != tc.status || !errors.Is(err, agent.ErrRuntime) || strings.Contains(fmt.Sprintf("%+v %#v", got, err), "credential-sentinel") {
				t.Fatalf("rejection/failure = %+v, %v", got, err)
			}
		})
	}
}

type eventSinkFunc func(agentevent.AgentEvent)

func (f eventSinkFunc) Emit(_ context.Context, event agentevent.AgentEvent) error {
	f(event)
	return nil
}

func TestSettledOutcomeAlwaysDeliversTerminalEvent(t *testing.T) {
	c, _ := fixture(t, pi.Options{}, func(runtime net.Conn, id string) { admit(runtime, id); settle(runtime, "stop") })
	// This is a 20,000-prompt stress budget, including race-detector overhead on
	// the two-vCPU Linux qualification wrapper, not an individual prompt deadline.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for range 20000 {
		var terminal atomic.Int32
		got, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) {
			if event.Kind == "prompt_settled" {
				terminal.Add(1)
			}
		}))
		if err != nil || got.Status != agent.Completed || terminal.Load() != 1 {
			t.Fatalf("settled outcome/event = %+v, %v, terminal events %d", got, err, terminal.Load())
		}
	}
}

func TestConfirmedAbortSettlementSurvivesEOF(t *testing.T) {
	for range 100 {
		started := make(chan struct{})
		c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) { admit(runtime, id) }, func(runtime net.Conn, id string) {
			response := map[string]any{"type": "response", "id": id, "command": "abort", "success": true}
			assistant := map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "aborted"}}
			var batch []byte
			for _, record := range []map[string]any{response, assistant, {"type": "agent_settled"}} {
				content, _ := json.Marshal(record)
				batch = append(batch, content...)
				batch = append(batch, '\n')
			}
			_, _ = runtime.Write(batch)
			_ = runtime.Close()
		})
		done := make(chan agent.Outcome, 1)
		go func() {
			got, _ := c.Prompt(ctx, "hold", eventSinkFunc(func(event agentevent.AgentEvent) {
				if event.Kind == "prompt_admitted" {
					close(started)
				}
			}))
			done <- got
		}()
		<-started
		err := c.Cancel(ctx)
		got := <-done
		if err != nil || got.Status != agent.Aborted {
			t.Fatalf("confirmed abort = %+v, %v", got, err)
		}
		_ = c.Close()
	}
}

func TestRetryCancellationMarkerDoesNotMisclassifyProviderFailures(t *testing.T) {
	for _, tc := range []struct {
		name, finalError string
		cancel           bool
	}{
		{"provider marker without cancellation", "Retry cancelled", false},
		{"abort racing retry failure", "controlled provider error credential-sentinel", true},
		{"provider text resembling marker", "Retry cancelled: credential-sentinel", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retrying := make(chan struct{})
			end := func(runtime net.Conn) {
				writeRecord(runtime, map[string]any{"type": "auto_retry_end", "success": false, "attempt": 1, "finalError": tc.finalError})
				writeRecord(runtime, map[string]any{"type": "agent_settled"})
			}
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				admit(runtime, id)
				writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error"}})
				writeRecord(runtime, map[string]any{"type": "auto_retry_start", "attempt": 1, "maxAttempts": 1, "delayMs": 30000, "errorMessage": "credential-sentinel"})
				if !tc.cancel {
					end(runtime)
				}
			}, func(runtime net.Conn, id string) {
				end(runtime)
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
			})
			var observations strings.Builder
			done := make(chan struct {
				outcome agent.Outcome
				err     error
			}, 1)
			go func() {
				got, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) {
					encoded, _ := json.Marshal(event)
					observations.Write(encoded)
					var metadata struct {
						Event string `json:"event"`
					}
					if json.Unmarshal(event.Metadata.Runtime, &metadata) == nil && metadata.Event == "auto_retry_start" {
						close(retrying)
					}
				}))
				done <- struct {
					outcome agent.Outcome
					err     error
				}{got, err}
			}()
			if tc.cancel {
				select {
				case <-retrying:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err := c.Cancel(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got := <-done
			if got.err != nil || got.outcome.Status != agent.Failed || got.outcome.FailureClass != "provider_failure" {
				t.Fatalf("provider outcome = %+v, %v", got.outcome, got.err)
			}
			if strings.Contains(observations.String()+fmt.Sprint(got), "credential-sentinel") {
				t.Fatal("retry error text leaked")
			}
		})
	}
}

func TestRetryBackoffCancellationStillRequiresSettlement(t *testing.T) {
	retrying := make(chan struct{})
	c, ctx := fixture(t, pi.Options{PromptTimeout: 150 * time.Millisecond, CancellationTimeout: 40 * time.Millisecond}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error"}})
		writeRecord(runtime, map[string]any{"type": "auto_retry_start", "attempt": 1, "maxAttempts": 1, "delayMs": 30000})
	}, func(runtime net.Conn, id string) {
		writeRecord(runtime, map[string]any{"type": "auto_retry_end", "success": false, "attempt": 1, "finalError": "Retry cancelled"})
		writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
	})
	done := make(chan struct {
		outcome agent.Outcome
		err     error
	}, 1)
	go func() {
		got, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) {
			var metadata struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(event.Metadata.Runtime, &metadata) == nil && metadata.Event == "auto_retry_start" {
				close(retrying)
			}
		}))
		done <- struct {
			outcome agent.Outcome
			err     error
		}{got, err}
	}()
	select {
	case <-retrying:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.Cancel(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel without settlement: %v", err)
	}
	got := <-done
	if !errors.Is(got.err, context.DeadlineExceeded) || got.outcome.Status != agent.Unknown {
		t.Fatalf("unsettled cancellation = %+v, %v", got.outcome, got.err)
	}
}
