package pi_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

func TestPrePromptCompactionUsesTurnDeadline(t *testing.T) {
	c, ctx := fixture(t, pi.Options{RequestTimeout: 100 * time.Millisecond}, func(runtime net.Conn, id string) {
		writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
		time.Sleep(200 * time.Millisecond)
		writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "aborted": false, "willRetry": false})
		admit(runtime, id)
		settle(runtime, "stop")
	})
	outcome, err := c.Prompt(ctx, "continue retained assignment", nil)
	if err != nil || outcome.Status != agent.Completed {
		t.Fatalf("pre-prompt compaction interrupted: %+v %v", outcome, err)
	}
}

func TestCompactionObservationsDoNotSettleAndFailuresAreSafe(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel", "retry"} {
		t.Run(mode, func(t *testing.T) {
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				admit(runtime, id)
				writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
				writeRecord(runtime, map[string]any{"type": "agent_end", "willRetry": false})
				writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
				end := map[string]any{"type": "compaction_end", "reason": "threshold", "aborted": mode == "cancel", "willRetry": mode == "retry"}
				if mode == "failure" {
					end["errorMessage"] = "Auto-compaction failed: private-provider-secret"
				}
				writeRecord(runtime, end)
				if mode == "retry" {
					writeRecord(runtime, map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
					writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
				}
				writeRecord(runtime, map[string]any{"type": "agent_settled"})
			})
			var events []agentevent.AgentEvent
			outcome, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) { events = append(events, event) }))
			want := agent.Completed
			if mode == "failure" {
				want = agent.Failed
			}
			if mode == "cancel" {
				want = agent.Aborted
			}
			if err != nil || outcome.Status != want {
				t.Fatalf("settlement: %+v %v", outcome, err)
			}
			count := 0
			for _, event := range events {
				if strings.Contains(string(event.Metadata.Runtime), "compaction_") {
					count++
				}
				if strings.Contains(string(event.Metadata.Runtime), "private-provider") {
					t.Fatal("provider error leaked")
				}
			}
			if count != 2 {
				t.Fatalf("compaction observations: %d", count)
			}
		})
	}
}

func TestPrePromptCompactionStillHonorsTurnDeadline(t *testing.T) {
	c, ctx := fixture(t, pi.Options{RequestTimeout: time.Second, CancellationTimeout: 50 * time.Millisecond}, func(runtime net.Conn, id string) {
		writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
	})
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	outcome, err := c.Prompt(short, "continue", nil)
	if !errors.Is(err, context.DeadlineExceeded) || outcome.Status != agent.Unknown || outcome.FailureClass != "deadline_exceeded" {
		t.Fatalf("deadline lost: %+v %v", outcome, err)
	}
}

func TestCancelledPrePromptCompactionCannotBecomeProviderFailure(t *testing.T) {
	started := make(chan struct{})
	var promptID string
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		promptID = id
		writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
		close(started)
		// Leave admission pending until the separate abort command is processed.
	}, func(runtime net.Conn, id string) {
		writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "aborted": true, "willRetry": false})
		// A prompt acknowledgement is still required after preflight cancellation.
		admit(runtime, promptID)
		writeRecord(runtime, map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		settle(runtime, "error")
		writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
	})
	done := make(chan agent.Outcome, 1)
	go func() { outcome, _ := c.Prompt(ctx, "continue", nil); done <- outcome }()
	<-started
	if err := c.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.Status != agent.Aborted {
			t.Fatalf("cancelled compaction: %+v", outcome)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestCompactionEndStillRequiresSettlementReceipt(t *testing.T) {
	compacted, finish := make(chan struct{}), make(chan struct{})
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
		writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
		writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "aborted": false, "willRetry": false})
		close(compacted)
		<-finish
		writeRecord(runtime, map[string]any{"type": "agent_settled"})
	})
	done := make(chan agent.Outcome, 1)
	go func() { outcome, _ := c.Prompt(ctx, "work", nil); done <- outcome }()
	<-compacted
	select {
	case outcome := <-done:
		close(finish)
		t.Fatalf("compaction prematurely settled: %+v", outcome)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	select {
	case outcome := <-done:
		if outcome.Status != agent.Completed {
			t.Fatalf("settlement: %+v", outcome)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestCancellationDuringCompactionCompatibilityWaitRequiresConfirmedSettlement(t *testing.T) {
	for _, tc := range []struct {
		name            string
		requestCancel   bool
		settlementAbort bool
		want            agent.Status
	}{
		{"confirmed cancellation", true, true, agent.Aborted},
		{"unsolicited flag", false, true, agent.Completed},
		{"completion wins cancellation race", true, false, agent.Completed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compacting, finish := make(chan struct{}), make(chan struct{})
			c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
				admit(runtime, id)
				writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
				writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
				close(compacting)
				if tc.requestCancel {
					return // The abort command releases the pending compatibility wait.
				}
				<-finish
				writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "aborted": false, "willRetry": false})
				writeRecord(runtime, map[string]any{"type": "agent_settled", "aborted": tc.settlementAbort})
			}, func(runtime net.Conn, id string) {
				writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "aborted": false, "willRetry": false})
				writeRecord(runtime, map[string]any{"type": "agent_settled", "aborted": tc.settlementAbort})
				writeRecord(runtime, map[string]any{"type": "response", "id": id, "command": "abort", "success": true})
			})
			done := make(chan agent.Outcome, 1)
			go func() { outcome, _ := c.Prompt(ctx, "continue original assignment", nil); done <- outcome }()
			<-compacting
			if tc.requestCancel {
				if err := c.Cancel(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				close(finish)
			}
			select {
			case outcome := <-done:
				if outcome.Status != tc.want {
					t.Fatalf("settlement: %+v; want %s", outcome, tc.want)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestCompactionFailureCannotInheritEarlierRequestDiagnostic(t *testing.T) {
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "omnigrexProviderDiagnostic": map[string]any{"stage": "provider_http", "http_status": 400}}})
		writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "overflow"})
		writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "overflow", "errorMessage": "compaction_failed"})
		writeRecord(runtime, map[string]any{"type": "agent_settled"})
	})
	var failed, terminal agentevent.AgentEvent
	outcome, err := c.Prompt(ctx, "continue", eventSinkFunc(func(event agentevent.AgentEvent) {
		if event.Kind == "provider_request_failed" {
			failed = event
		}
		if event.Kind == "prompt_settled" {
			terminal = event
		}
	}))
	if err != nil || outcome.Status != agent.Failed || outcome.FailureClass != "provider_failure" {
		t.Fatalf("settlement: %+v %v", outcome, err)
	}
	if !strings.Contains(string(failed.Metadata.Runtime), `"http_status":400`) {
		t.Fatal("original request evidence lost")
	}
	if outcome.FailureDiagnostic != nil || strings.Contains(string(terminal.Metadata.Runtime), `"http_status":400`) {
		t.Fatalf("summary inherited previous request evidence: %s", terminal.Metadata.Runtime)
	}
}

func TestSummaryRequestDiagnosticReachesOperationalEvent(t *testing.T) {
	for _, category := range []string{"subscription_temporary", "subscription_quota", "subscription_auth", "subscription_configuration"} {
		for _, nextSummary := range []bool{false, true} {
			t.Run(category+"/"+map[bool]string{false: "own evidence", true: "no stale summary evidence"}[nextSummary], func(t *testing.T) {
				c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
					admit(runtime, id)
					writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
					writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
					writeRecord(runtime, map[string]any{"type": "omnigrex_provider_request_failed", "failureClass": category, "diagnostic": map[string]any{
						"stage": "provider_http", "cause": "http", "http_status": 502, "code": "subscription_sharing_usage_unavailable",
						"attempt": "00000000-0000-4000-8000-000000000001", "request_id_hash": strings.Repeat("a", 64), "message": "SENTINEL",
					}})
					writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "errorMessage": "Omnigrex compaction_failed"})
					if nextSummary {
						writeRecord(runtime, map[string]any{"type": "compaction_start", "reason": "threshold"})
						writeRecord(runtime, map[string]any{"type": "compaction_end", "reason": "threshold", "errorMessage": "Omnigrex compaction_failed"})
					}
					writeRecord(runtime, map[string]any{"type": "agent_settled"})
				})
				requests := 0
				outcome, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) {
					if strings.Contains(string(event.Metadata.Runtime), "SENTINEL") {
						t.Error("raw summary metadata escaped")
					}
					if event.Kind == "provider_request_failed" {
						requests++
						for _, want := range []string{`"stage":"provider_http"`, `"http_status":502`, `"code":"subscription_sharing_usage_unavailable"`, `"attempt":"00000000-0000-4000-8000-000000000001"`, `"request_id_hash":"` + strings.Repeat("a", 64) + `"`} {
							if !strings.Contains(string(event.Metadata.Runtime), want) {
								t.Errorf("summary event lost %s: %s", want, event.Metadata.Runtime)
							}
						}
					}
				}))
				if err != nil || outcome.Status != agent.Failed || outcome.FailureClass != "provider_failure" || requests != 1 {
					t.Fatalf("summary failure = %+v, %v; operational requests=%d", outcome, err, requests)
				}
				if nextSummary {
					if outcome.FailureDiagnostic != nil {
						t.Fatalf("stale summary evidence: %+v", outcome.FailureDiagnostic)
					}
				} else if outcome.FailureDiagnostic == nil || outcome.FailureDiagnostic.HTTPStatus == nil || *outcome.FailureDiagnostic.HTTPStatus != 502 || !strings.Contains(outcome.FailureDiagnostic.Summary(), "category="+category) {
					t.Fatalf("lost summary evidence/category: %+v", outcome.FailureDiagnostic)
				}
			})
		}
	}
}

func TestRequestObservationDoesNotDuplicateConversationalFailure(t *testing.T) {
	diagnostic := map[string]any{"attempt": "00000000-0000-4000-8000-000000000001", "stage": "transport", "cause": "timeout"}
	c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
		admit(runtime, id)
		writeRecord(runtime, map[string]any{"type": "message_start", "message": map[string]string{"role": "assistant"}})
		writeRecord(runtime, map[string]any{"type": "omnigrex_provider_request_failed", "failureClass": "subscription_temporary", "diagnostic": diagnostic})
		writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "aborted", "omnigrexProviderDiagnostic": diagnostic}})
		writeRecord(runtime, map[string]any{"type": "agent_settled"})
	})
	requests := 0
	outcome, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) {
		if event.Kind == "provider_request_failed" {
			requests++
		}
	}))
	if err != nil || outcome.Status != agent.Aborted || outcome.FailureDiagnostic == nil || outcome.FailureDiagnostic.Cause != "timeout" || requests != 1 {
		t.Fatalf("admitted timeout: %+v, %v; requests=%d", outcome, err, requests)
	}
}

func TestMalformedSummaryObservationCannotChangeSettlement(t *testing.T) {
	for _, diagnostic := range []any{"SENTINEL", map[string]any{"stage": "provider_http", "code": strings.Repeat("SENTINEL", 1000)}} {
		c, ctx := fixture(t, pi.Options{}, func(runtime net.Conn, id string) {
			admit(runtime, id)
			writeRecord(runtime, map[string]any{"type": "message_end", "message": map[string]string{"role": "assistant", "stopReason": "stop"}})
			writeRecord(runtime, map[string]any{"type": "compaction_start"})
			writeRecord(runtime, map[string]any{"type": "omnigrex_provider_request_failed", "failureClass": map[string]string{"message": "SENTINEL"}, "diagnostic": diagnostic})
			writeRecord(runtime, map[string]any{"type": "compaction_end", "errorMessage": "Omnigrex compaction_failed"})
			writeRecord(runtime, map[string]any{"type": "agent_settled"})
		})
		requests := 0
		outcome, err := c.Prompt(ctx, "work", eventSinkFunc(func(event agentevent.AgentEvent) {
			if strings.Contains(string(event.Metadata.Runtime), "SENTINEL") || strings.Contains(event.Metadata.FailureClass, "SENTINEL") {
				t.Error("malformed diagnostic escaped")
			}
			if event.Kind == "provider_request_failed" {
				requests++
			}
		}))
		if err != nil || outcome.Status != agent.Failed || outcome.FailureClass != "provider_failure" || outcome.FailureDiagnostic == nil || outcome.FailureDiagnostic.Stage != "unknown" || requests != 1 {
			t.Fatalf("diagnostic changed native settlement: %+v, %v; requests=%d", outcome, err, requests)
		}
	}
}
