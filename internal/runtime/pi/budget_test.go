package pi_test

import (
	"errors"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/stepbudget"
	"net"
	"testing"
)

func TestPiBudgetEvidenceTranslation(t *testing.T) {
	for _, reason := range []string{"stop", "toolUse", "length", "error", "aborted"} {
		t.Run(reason, func(t *testing.T) {
			limit, _ := stepbudget.Parse("1")
			c, ctx := fixture(t, pi.Options{StepBudget: &limit}, func(runtime net.Conn, id string) {
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "prompt", "success": true, "data": map[string]any{"disposition": "started"}})
				b := stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: reason == "stop" || reason == "toolUse", Complete: true}
				writeRecord(runtime, map[string]any{"type": "omnigrex_step_budget", "data": b, "summary": "unfinished work"})
				settle(runtime, reason)
			})
			got, err := c.Prompt(ctx, "work", nil)
			if err != nil || got.StepBudget == nil || got.StepBudget.Used != 1 || !got.StepBudget.Complete {
				t.Fatalf("%+v %v", got, err)
			}
			if reason == "toolUse" && got.Status != agent.Completed {
				t.Fatal(got)
			}
			if reason == "length" && !got.LimitReached {
				t.Fatal(got)
			}
			if reason == "error" && got.Status != agent.Failed {
				t.Fatal(got)
			}
			if reason == "aborted" && got.Status != agent.Aborted {
				t.Fatal(got)
			}
		})
	}
}
func TestPiBudgetRejectsUnboundedOrMissingEvidence(t *testing.T) {
	for _, raw := range []any{nil, map[string]any{"limit": 1, "used": 2, "responses": 2, "summary": true, "stopped": true, "complete": true}} {
		t.Run("invalid", func(t *testing.T) {
			limit, _ := stepbudget.Parse("1")
			c, ctx := fixture(t, pi.Options{StepBudget: &limit}, func(runtime net.Conn, id string) {
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "prompt", "success": true, "data": map[string]any{"disposition": "started"}})
				if raw != nil {
					writeRecord(runtime, map[string]any{"type": "omnigrex_step_budget", "data": raw})
				}
				settle(runtime, "stop")
			})
			got, err := c.Prompt(ctx, "work", nil)
			if !errors.Is(err, agent.ErrProtocol) || got.Status != agent.Unknown {
				t.Fatalf("%+v %v", got, err)
			}
			if got.StepBudget == nil || got.StepBudget.Complete {
				t.Fatal("missing observation claimed complete", got)
			}
		})
	}
}

func TestPiTransportLossRetainsPartialUsageInsteadOfInventingZero(t *testing.T) {
	limit, _ := stepbudget.Parse("3")
	c, ctx := fixture(t, pi.Options{StepBudget: &limit}, func(runtime net.Conn, id string) {
		writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "prompt", "success": true, "data": map[string]any{"disposition": "started"}})
		writeRecord(runtime, map[string]any{"type": "omnigrex_step_budget", "data": stepbudget.Observation{Limit: limit, Used: 1}})
		_ = runtime.Close()
	})
	got, err := c.Prompt(ctx, "work", nil)
	if err == nil || got.Status != agent.Unknown || got.StepBudget == nil || got.StepBudget.Used != 1 || got.StepBudget.Responses != 0 || got.StepBudget.Complete || got.StepBudget.Stopped {
		t.Fatalf("lost transport usage = %+v %v", got, err)
	}
}

func TestBudgetStopDoesNotReplaceCancellationSettlementAuthority(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion wins race", true: "confirmed cancellation"}[confirmed], func(t *testing.T) {
			limit, _ := stepbudget.Parse("1")
			ready := make(chan struct{})
			c, ctx := fixture(t, pi.Options{StepBudget: &limit}, func(runtime net.Conn, id string) {
				admit(runtime, id)
				writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
				writeRecord(runtime, map[string]any{"type": "omnigrex_step_budget", "data": stepbudget.Observation{Limit: limit, Used: 1, Responses: 1, Summary: true, Stopped: true, Complete: true}})
				close(ready)
			}, func(runtime net.Conn, id string) {
				writeRecord(runtime, map[string]any{"type": "agent_settled", "aborted": confirmed})
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
			})
			done := make(chan agent.Outcome, 1)
			go func() { outcome, _ := c.Prompt(ctx, "work", nil); done <- outcome }()
			<-ready
			if err := c.Cancel(ctx); err != nil {
				t.Fatal(err)
			}
			want := agent.Completed
			if confirmed {
				want = agent.Aborted
			}
			select {
			case outcome := <-done:
				if outcome.Status != want || outcome.StepBudget == nil || !outcome.StepBudget.Complete {
					t.Fatalf("settlement = %+v; want %s", outcome, want)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
