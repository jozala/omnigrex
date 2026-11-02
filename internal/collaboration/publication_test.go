package collaboration_test

import (
	"context"
	"testing"

	"github.com/jozala/omnigrex/internal/collaboration"
)

type webPublisher struct {
	state  *collaboration.State
	posts  int
	source collaboration.Source
}

func (p *webPublisher) Validate(collaboration.PublicationMessage) error { return nil }
func (p *webPublisher) Publish(_ context.Context, message collaboration.PublicationMessage) (collaboration.Source, error) {
	if len(p.state.Rounds) != 1 || p.state.Rounds[0].ID != message.Round.ID || message.WorkflowID != "work-item-workflow" {
		panic("unreserved publication")
	}
	p.posts++
	p.source = collaboration.Source{Channel: "web", ID: "document-1"}
	return p.source, context.DeadlineExceeded
}
func (p *webPublisher) Reconcile(context.Context, collaboration.PublicationMessage) (collaboration.Source, bool, error) {
	return p.source, p.source.ID != "", nil
}
func TestChannelNeutralPublicationRecoversWithoutRepeatingDelivery(t *testing.T) {
	var state collaboration.State
	publisher := &webPublisher{state: &state}
	operation := collaboration.Publication{WorkflowID: "work-item-workflow", Publisher: publisher, Confirm: func(_ context.Context, id string, source collaboration.Source) error {
		return state.Publish(id, source)
	}}
	round, err := operation.Publish(context.Background(), func(context.Context) (collaboration.Round, error) {
		return state.Request("R1", "developer", "T1", "head", []collaboration.Question{{ID: "Q1", Body: "Policy?"}})
	}, collaboration.Round{Questions: []collaboration.Question{{ID: "Q1", Body: "Policy?"}}}, "")
	if err == nil || state.Rounds[0].Published {
		t.Fatal("uncertain delivery confirmed")
	}
	found, err := operation.Reconcile(context.Background(), round, "")
	if err != nil || !found || !state.Rounds[0].Published || state.Rounds[0].Source.Channel != "web" || publisher.posts != 1 {
		t.Fatal(state, found, err)
	}
}
