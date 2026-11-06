package collaboration

import (
	"context"
	"errors"
)

var ErrPublicationTooLarge = errors.New("complete publication exceeds the transport limit")

// PublicationMessage carries application scope without transport identities.
type PublicationMessage struct {
	Artifact   *RenderedPublication
	WorkflowID string
	Checkpoint *PlanningRevision
	Round      Round
	Signature  string
}

// RenderedPublication retains the complete posted payload (Body includes any
// signature and marker). Marker is an identity index, not text to append again.
type RenderedPublication struct {
	Body   string `json:"body"`
	Marker string `json:"marker"`
}

// Renderer freezes the complete adapter payload including signature and marker.
type Renderer interface {
	Render(PublicationMessage) (RenderedPublication, error)
}

// Publisher owns credentials, delivery and exact artifact reconciliation.
// Not found is uncertain, not permission to repeat a mutation.
type Publisher interface {
	Validate(PublicationMessage) error
	Publish(context.Context, PublicationMessage) (Source, error)
	Reconcile(context.Context, PublicationMessage) (Source, bool, error)
}

// Publication is the channel-neutral application operation. Reservation precedes
// the external effect, and only verified delivery is confirmed durably.
type Publication struct {
	WorkflowID       string
	Publisher        Publisher
	Confirm          func(context.Context, string, Source) error
	SaveArtifact     func(context.Context, string, RenderedPublication) error
	RejectCheckpoint func(context.Context, string, string) error
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
	m := PublicationMessage{Artifact: round.Artifact, WorkflowID: p.WorkflowID, Round: round, Signature: signature}
	if err = p.freeze(ctx, round.ID, &m); err != nil {
		return round, err
	}
	source, err := p.Publisher.Publish(ctx, m)
	if err != nil {
		return round, err
	}
	return round, p.Confirm(ctx, round.ID, source)
}
func (p Publication) Reconcile(ctx context.Context, round Round, signature string) (bool, error) {
	if _, renders := p.Publisher.(Renderer); renders && round.Artifact == nil {
		return false, nil
	}
	source, found, err := p.Publisher.Reconcile(ctx, PublicationMessage{Artifact: round.Artifact, WorkflowID: p.WorkflowID, Round: round, Signature: signature})
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
	m := PublicationMessage{Artifact: r.Artifact, WorkflowID: p.WorkflowID, Checkpoint: &r, Signature: signature}
	if err = p.Publisher.Validate(m); err != nil {
		if errors.Is(err, ErrPublicationTooLarge) && p.RejectCheckpoint != nil {
			if rejectErr := p.RejectCheckpoint(ctx, r.ID, "payload_too_large"); rejectErr != nil {
				return r, rejectErr
			}
			return r, &CheckpointNotApplied{OperationID: r.ID, ParticipantID: r.ParticipantID, TurnID: r.TurnID, Content: r.Content, Reason: "payload_too_large"}
		}
		return r, err
	}
	if err = p.freeze(ctx, r.ID, &m); err != nil {
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
	if _, renders := p.Publisher.(Renderer); renders && r.Artifact == nil {
		return false, nil
	}
	source, found, err := p.Publisher.Reconcile(ctx, PublicationMessage{Artifact: r.Artifact, WorkflowID: p.WorkflowID, Checkpoint: &r, Signature: signature})
	if err != nil || !found {
		return false, err
	}
	return true, p.Confirm(ctx, r.ID, source)
}

func (p Publication) freeze(ctx context.Context, id string, m *PublicationMessage) error {
	renderer, ok := p.Publisher.(Renderer)
	if !ok {
		return nil
	}
	if m.Artifact != nil {
		return nil
	}
	if p.SaveArtifact == nil {
		return ErrInvalid
	}
	artifact, err := renderer.Render(*m)
	if err != nil {
		return err
	}
	if err = p.SaveArtifact(ctx, id, artifact); err != nil {
		return err
	}
	m.Artifact = &artifact
	return nil
}
