//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jozala/omnigrex/internal/store"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestReplanningTransferredQuestionLifecycle(t *testing.T) {
	for i, scenario := range []string{"answer-after-transfer", "reject-omitted-unresolved", "carry-unresolved", "inherit-settled"} {
		t.Run(scenario, func(t *testing.T) {
			dbs, pool := openPhaseFiveStores(t, 2)
			db := dbs[0]
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fixture, developer, spec := reviewReplanningDeveloper(t, db, pool, ctx, 2490+i)
			round := reviewReplanningQuestion(t, db, ctx, developer)
			command := collaboration.Command{Kind: "steer", Body: "Investigate whether restart requirements conflict with the retention commitment."}
			if scenario == "inherit-settled" {
				command = collaboration.Command{Kind: "answer", Target: round.Reference, Body: "Only nonpersonal decisions may be retained."}
			}
			input := reviewReplanningInput(t, db, ctx, fixture.workflowID, command)
			developer = reviewReplanningContinue(t, db, pool, ctx, developer, spec, workflow.TurnOutcomeClarificationWait)
			reviewReplanningAssessment(t, db, ctx, developer, input, "unchanged")
			if scenario == "inherit-settled" {
				resolutions := []collaboration.Resolution{{QuestionID: "retention", ResponseIDs: []string{input}, Explanation: "Only nonpersonal decisions are allowed."}}
				recordReplanningMutation(t, db, ctx, developer, "settled-question", "resolve_clarification", map[string]any{"round_id": round.ID, "resolutions": resolutions}, func(op string) any {
					if err := db.ResolveClarification(ctx, developer, op, round.ID, resolutions); err != nil {
						t.Fatal(err)
					}
					return map[string]any{"recorded": true}
				})
			}
			planner, spec := reviewReplanningConcern(t, db, pool, ctx, fixture, developer, spec)
			state := replanningState(t, pool, ctx, fixture.workflowID)
			if state.CurrentRound != round.ID || !state.RoundInScope(round, planner.AgentParticipantID) {
				t.Fatal("transferred round not current/in scope")
			}
			if scenario == "answer-after-transfer" {
				// A reply crosses responsibility transfer, not a question replacement.
				input = reviewReplanningInput(t, dbs[1], ctx, fixture.workflowID, collaboration.Command{Kind: "answer", Target: round.Reference, Body: "Only nonpersonal decisions may be retained."})
				planner = reviewReplanningContinue(t, db, pool, ctx, planner, spec, workflow.TurnOutcomeClarificationWait)
				state = replanningState(t, pool, ctx, fixture.workflowID)
				for _, in := range state.Inputs {
					if in.ID == input && (in.ParticipantID != planner.AgentParticipantID || in.RoundID != round.ID || in.DeliveredTo != planner.ID) {
						t.Fatal("answer misrouted", in)
					}
				}
				reviewReplanningAssessment(t, db, ctx, planner, input, "unchanged")
				resolutions := []collaboration.Resolution{{QuestionID: "retention", ResponseIDs: []string{input}, Explanation: "The authorized answer settles the transferred question."}}
				recordReplanningMutation(t, db, ctx, planner, "resolve-transferred", "resolve_clarification", map[string]any{"round_id": round.ID, "resolutions": resolutions}, func(op string) any {
					if err := db.ResolveClarification(ctx, planner, op, round.ID, resolutions); err != nil {
						t.Fatal(err)
					}
					return map[string]any{"recorded": true}
				})
				state = replanningState(t, pool, ctx, fixture.workflowID)
				if state.Unresolved(planner.AgentParticipantID) {
					t.Fatal("answered question remains unresolved")
				}
			} else {
				questions := []collaboration.Question{{ID: "title", Body: "Which documentation title should be used?"}}
				if scenario != "reject-omitted-unresolved" {
					questions = append(questions, round.Questions...)
				}
				// Reserve and apply through the Store; rejected reservations cannot supersede
				// the current question or make an unresolved product decision disappear.
				var replacement collaboration.Round
				var applicationErr error
				recordReplanningMutation(t, db, ctx, planner, "replacement-"+uuid.NewString(), "request_clarification", map[string]any{"questions": questions}, func(op string) any {
					replacement, applicationErr = db.RequestClarification(ctx, planner, op, questions)
					if scenario == "reject-omitted-unresolved" {
						if !errors.Is(applicationErr, collaboration.ErrInvalid) {
							t.Errorf("omitted transferred unresolved question accepted: %v", applicationErr)
						}
						return map[string]any{"recorded": false}
					}
					if applicationErr != nil {
						t.Fatal(applicationErr)
					}
					if err := db.ConfirmClarificationPublication(ctx, fixture.workflowID, replacement.ID, collaboration.Source{Channel: "fixture", ID: "replacement"}); err != nil {
						t.Fatal(err)
					}
					return map[string]any{"round_id": replacement.ID}
				})
				state = replanningState(t, pool, ctx, fixture.workflowID)
				if scenario == "reject-omitted-unresolved" {
					if state.CurrentRound != round.ID || !state.Unresolved(planner.AgentParticipantID) || state.ResumptionReady() {
						t.Fatal("omission removed unresolved prerequisite")
					}
				} else if scenario == "carry-unresolved" {
					if !state.Unresolved(planner.AgentParticipantID) || len(replacement.Resolutions) != 0 {
						t.Fatal("carried question acquired a decision")
					}
				} else {
					if len(replacement.Resolutions) != 1 || replacement.Resolutions[0].QuestionID != "retention" || len(replacement.Resolutions[0].ResponseIDs) != 1 || replacement.Resolutions[0].ResponseIDs[0] != input {
						t.Fatal("settled decision not inherited", replacement.Resolutions)
					}
				}
				if scenario == "carry-unresolved" || scenario == "inherit-settled" {
					answer := reviewReplanningInput(t, dbs[1], ctx, fixture.workflowID, collaboration.Command{Kind: "answer", Target: replacement.Reference, Body: "Use the title Restart decisions."})
					planner = reviewReplanningContinue(t, db, pool, ctx, planner, spec, workflow.TurnOutcomeClarificationWait)
					reviewReplanningAssessment(t, db, ctx, planner, answer, "unchanged")
					resolutions := []collaboration.Resolution{{QuestionID: "title", ResponseIDs: []string{answer}, Explanation: "The title is Restart decisions."}}
					recordReplanningMutation(t, db, ctx, planner, "resolve-title", "resolve_clarification", map[string]any{"round_id": replacement.ID, "resolutions": resolutions}, func(op string) any {
						if err := db.ResolveClarification(ctx, planner, op, replacement.ID, resolutions); err != nil {
							t.Fatal(err)
						}
						return map[string]any{"recorded": true}
					})
					state = replanningState(t, pool, ctx, fixture.workflowID)
					if state.Unresolved(planner.AgentParticipantID) != (scenario == "carry-unresolved") {
						t.Fatal("title resolution lost retention prerequisite or inherited decision")
					}
					if scenario == "carry-unresolved" {
						_, err := db.ReserveMutation(ctx, planner, store.MutationSpec{OperationID: "premature-completion", ToolName: "complete_planning", Request: json.RawMessage(`{"operation_id":"premature-completion"}`)})
						if !errors.Is(err, store.ErrUnresolvedClarification) || state.ResumptionReady() {
							t.Fatal("unanswered carried question permits resumption", err)
						}
					}
					receipt, err := dbs[1].AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "answer", Target: round.Reference, Body: "Late superseded reply"}, Authorized: true, Actor: collaboration.Actor{ID: "42"}, Source: collaboration.Source{Channel: "fixture", ID: uuid.NewString()}, At: time.Now()})
					if err != nil || receipt.ResponseID != "" {
						t.Fatal("superseded transferred round accepted", receipt, err)
					}
				}
			}
			old, err := dbs[1].GetClarificationRound(ctx, fixture.workflowID, round.ID)
			if err != nil || old.ParticipantID != developer.AgentParticipantID {
				t.Fatal("original round reparented", old, err)
			}
		})
	}
}
