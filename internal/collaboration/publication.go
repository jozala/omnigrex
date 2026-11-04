package collaboration

import "context"

// PublicationMessage carries application scope without transport identities.
type PublicationMessage struct {
	WorkflowID string
	Checkpoint *PlanningRevision
	Round      Round
	Signature  string
}

// Publisher owns transport rendering, credentials, delivery and exact artifact
// reconciliation. Not found is uncertain, not permission to repeat a mutation.
type Publisher interface {
	Validate(PublicationMessage) error
	Publish(context.Context, PublicationMessage) (Source, error)
	Reconcile(context.Context, PublicationMessage) (Source, bool, error)
}

// Publication is the channel-neutral application operation. Reservation precedes
// the external effect, and only verified delivery is confirmed durably.
type Publication struct {
	WorkflowID string
	Publisher  Publisher
	Confirm    func(context.Context, string, Source) error
}

func (p Publication) Publish(ctx context.Context, reserve func(context.Context) (Round, error), provisional Round, signature string) (Round, error) {
	if len(provisional.Questions) > 0 {
		if err := p.Publisher.Validate(PublicationMessage{WorkflowID: p.WorkflowID, Round: provisional, Signature: signature}); err != nil {
			return Round{}, err
		}
	}
	round, err := reserve(ctx)
	if err != nil || round.ID == "" {
		return round, err
	}
	source, err := p.Publisher.Publish(ctx, PublicationMessage{WorkflowID: p.WorkflowID, Round: round, Signature: signature})
	if err != nil {
		return round, err
	}
	return round, p.Confirm(ctx, round.ID, source)
}
func (p Publication) Reconcile(ctx context.Context, round Round, signature string) (bool, error) {
	source, found, err := p.Publisher.Reconcile(ctx, PublicationMessage{WorkflowID: p.WorkflowID, Round: round, Signature: signature})
	if err != nil || !found {
		return false, err
	}
	if err = p.Confirm(ctx, round.ID, source); err != nil {
		return false, err
	}
	return true, nil
}

func (p Publication) PublishCheckpoint(ctx context.Context, reserve func(context.Context) (PlanningRevision, error), signature string) (PlanningRevision, error) {
	r, err := reserve(ctx)
	if err != nil {
		return r, err
	}
	m := PublicationMessage{WorkflowID: p.WorkflowID, Checkpoint: &r, Signature: signature}
	if err = p.Publisher.Validate(m); err != nil {
		return r, err
	}
	source, err := p.Publisher.Publish(ctx, m)
	if err != nil {
		return r, err
	}
	if err = p.Confirm(ctx, r.ID, source); err != nil {
		return r, err
	}
	r.Published = true
	r.Source = &source
	return r, nil
}
func (p Publication) ReconcileCheckpoint(ctx context.Context, r PlanningRevision, signature string) (bool, error) {
	source, found, err := p.Publisher.Reconcile(ctx, PublicationMessage{WorkflowID: p.WorkflowID, Checkpoint: &r, Signature: signature})
	if err != nil || !found {
		return false, err
	}
	return true, p.Confirm(ctx, r.ID, source)
}
