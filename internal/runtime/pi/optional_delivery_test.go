package pi_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/stepbudget"
)

func writeTemporarySample(runtime net.Conn) {
	writeRecord(runtime, map[string]any{"type": "omnigrex_temporary_filesystems", "data": []any{
		map[string]any{"resource": "tmp", "status": "complete", "used": 42},
		map[string]any{"resource": "run-pi-agent", "status": "unavailable"},
	}})
}

func TestOptionalFilesystemDeliveryAcceptOrRejectDoesNotChangeSettlement(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accept", true: "reject"}[reject], func(t *testing.T) {
			entered := make(chan struct{})
			var optional, terminal, aborts atomic.Int32
			var invalid atomic.Bool
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				admit(runtime, id)
				writeTemporarySample(runtime)
				<-entered // Force optional delivery before settlement, not a lucky drop.
				settle(runtime, "stop")
			}, func(runtime net.Conn, id string) {
				aborts.Add(1)
				settle(runtime, "aborted")
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
			})
			sink := historySinkFunc(func(_ context.Context, e agentevent.AgentEvent) error {
				if e.Kind == "temporary_filesystems" {
					if len(e.Metadata.TemporaryFilesystems) != 2 {
						invalid.Store(true)
					}
					if optional.Add(1) == 1 {
						close(entered)
					}
					if reject {
						return errors.New("optional sink rejected")
					}
					return nil
				}
				if e.Kind == "prompt_settled" {
					terminal.Add(1)
				}
				return nil
			})
			for range 2 {
				got, err := c.Prompt(ctx, "optional rejection", sink)
				if err != nil || got.Status != agent.Completed {
					t.Fatalf("optional rejection interrupted prompt: %+v %v", got, err)
				}
			}
			if invalid.Load() {
				t.Fatal("optional event lost its validated two-mount shape")
			}
			if optional.Load() < 1 || terminal.Load() != 2 || aborts.Load() != 0 {
				t.Fatalf("optional=%d terminal=%d aborts=%d", optional.Load(), terminal.Load(), aborts.Load())
			}
		})
	}
}

func TestStalledOptionalDeliveryDoesNotDelaySettlementBudgetOrLaterPrompts(t *testing.T) {
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan string, 1)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls, terminal, aborts atomic.Int32
	optionalCtx := make(chan context.Context, 1)
	limit, _ := stepbudget.Parse("1")
	c, ctx := fixture(t, pi.Options{StepBudget: &limit}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeTemporarySample(runtime)
		<-entered
		for range 200 {
			writeTemporarySample(runtime)
		}
		writeRecord(runtime, map[string]any{"type": "omnigrex_step_budget", "data": stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: true, Complete: true}})
		settle(runtime, "stop")
	}, func(runtime net.Conn, id string) {
		aborts.Add(1)
		settle(runtime, "aborted")
		writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
	})
	sink := historySinkFunc(func(deliveryCtx context.Context, e agentevent.AgentEvent) error {
		if e.Kind == "temporary_filesystems" {
			if calls.Add(1) == 1 {
				optionalCtx <- deliveryCtx
				close(entered)
				<-release // Deliberately ignore cancellation to test the resource bound.
				returned <- e.TurnID
			}
			return nil
		}
		if e.Kind == "prompt_settled" {
			terminal.Add(1)
		}
		return nil
	})
	// Reusing the connection must not launch a worker per prompt behind the stall.
	for _, turnID := range []string{"first", "second", "third"} {
		c.SetAgentEventContext(agentevent.Context{TurnID: turnID})
		got, err := c.Prompt(ctx, "optional stall", sink)
		if err != nil || got.Status != agent.Completed {
			t.Fatalf("stalled optional sink delayed settlement: %+v %v", got, err)
		}
		if got.StepBudget == nil || !got.StepBudget.Complete || got.StepBudget.Used != 1 || !got.StepBudget.Stopped {
			t.Fatalf("lost independent budget snapshot: %+v", got)
		}
	}
	if calls.Load() != 1 || terminal.Load() != 3 || aborts.Load() != 0 {
		t.Fatalf("calls=%d terminal=%d aborts=%d", calls.Load(), terminal.Load(), aborts.Load())
	}
	select {
	case <-(<-optionalCtx).Done():
	case <-ctx.Done():
		t.Fatal("optional context not cancelled after prompt")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case turnID := <-returned:
		if turnID != "first" {
			t.Fatalf("late sample rebound to Turn %q", turnID)
		}
	case <-time.After(time.Second):
		t.Fatal("optional worker did not return after release")
	}
}

func TestOptionalDeliveryDoesNotWeakenCriticalSinkRejection(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) { admit(runtime, id); settle(runtime, "stop") })
	got, err := c.Prompt(ctx, "critical rejection", historySinkFunc(func(_ context.Context, e agentevent.AgentEvent) error {
		if e.Kind == "prompt_settled" {
			return errors.New("critical sink rejected")
		}
		return nil
	}))
	if !errors.Is(err, agent.ErrBackpressure) || got.Status != agent.Unknown {
		t.Fatalf("critical rejection no longer fails: %+v %v", got, err)
	}
	if _, err := c.Prompt(ctx, "must not reuse", nil); err == nil {
		t.Fatal("critical failure left connection usable")
	}
}

func TestStalledOptionalDeliveryDoesNotDelayExplicitCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeTemporarySample(runtime)
		<-entered
	})
	type result struct {
		outcome agent.Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		got, err := c.Prompt(ctx, "cancel with optional stall", historySinkFunc(func(_ context.Context, e agentevent.AgentEvent) error {
			if e.Kind == "temporary_filesystems" {
				close(entered)
				<-release
			}
			return nil
		}))
		done <- result{got, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("optional sink never entered")
	}
	if err := c.Cancel(ctx); err != nil {
		t.Fatalf("cancellation blocked by optional delivery: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.outcome.Status != agent.Aborted {
			t.Fatalf("cancelled outcome: %+v %v", got.outcome, got.err)
		}
	case <-ctx.Done():
		t.Fatal("optional delivery delayed confirmed cancellation")
	}
}
