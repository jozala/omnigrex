// Package collaboration defines Work Item human interaction independently of its transport.
// It does not grant Session control or implementation authorization.
package collaboration

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Purpose string

const Clarification Purpose = "CLARIFICATION"

type Source struct {
	Channel string `json:"channel"`
	ID      string `json:"id"`
	URL     string `json:"url,omitempty"`
}
type Actor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Question struct {
	Replaces string `json:"replaces,omitempty"`
	ID       string `json:"id"`
	Body     string `json:"body"`
}
type Resolution struct {
	Obsolete    bool     `json:"obsolete,omitempty"`
	QuestionID  string   `json:"question_id"`
	ResponseIDs []string `json:"response_ids"`
	Explanation string   `json:"explanation"`
}
type Round struct {
	Supersedes    string       `json:"supersedes,omitempty"`
	ID            string       `json:"id"`
	Purpose       Purpose      `json:"purpose"`
	ParticipantID string       `json:"participant_id"`
	TurnID        string       `json:"turn_id"`
	HeadSHA       string       `json:"head_sha"`
	Questions     []Question   `json:"questions"`
	Resolutions   []Resolution `json:"resolutions,omitempty"`
	SupersededBy  string       `json:"superseded_by,omitempty"`
	Published     bool         `json:"published"`
	Source        *Source      `json:"source,omitempty"`
}
type Input struct {
	Route         string    `json:"route,omitempty"`
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	RoundID       string    `json:"round_id,omitempty"`
	Amends        string    `json:"amends,omitempty"`
	Body          string    `json:"body"`
	Actor         Actor     `json:"actor"`
	Source        Source    `json:"source"`
	AcceptedAt    time.Time `json:"accepted_at"`
	ParticipantID string    `json:"participant_id"`
	DeliveredTo   string    `json:"delivered_to,omitempty"`
}
type Receipt struct {
	ID         string `json:"id"`
	Source     Source `json:"source"`
	ResponseID string `json:"response_id,omitempty"`
	Feedback   string `json:"feedback"`
}
type Preparation struct {
	ParticipantID string `json:"participant_id"`
	TurnID        string `json:"turn_id"`
}

type State struct {
	Preparations []Preparation `json:"preparations,omitempty"`
	Rounds       []Round       `json:"rounds"`
	Inputs       []Input       `json:"inputs"`
	Receipts     []Receipt     `json:"receipts"`
	CurrentRound string        `json:"current_round,omitempty"`
	Waiting      bool          `json:"waiting"`
}
type Command struct {
	Kind   string
	Target string
	Body   string
}
type Admission struct {
	Route         string
	Active        bool
	Authorized    bool
	ParticipantID string
	Actor         Actor
	Source        Source
	At            time.Time
}

var ErrInvalid = errors.New("invalid collaboration operation")

// Request reserves an immutable question round before publication. An unconfirmed
// publication is never represented as delivered. The caller supplies domain IDs.
func (s *State) Request(id, participant, turn, head string, questions []Question) (Round, error) {
	if id == "" || participant == "" || turn == "" || len(questions) == 0 {
		return Round{}, ErrInvalid
	}
	for _, r := range s.Rounds {
		if !r.Published {
			return Round{}, ErrInvalid
		}
		if r.ID == id {
			return Round{}, ErrInvalid
		}
	}
	seen := map[string]bool{}
	for _, q := range questions {
		if q.ID == "" || strings.TrimSpace(q.Body) == "" || seen[q.ID] {
			return Round{}, ErrInvalid
		}
		seen[q.ID] = true
		// A stable question identity must never acquire materially different content.
		for _, r := range s.Rounds {
			for _, old := range r.Questions {
				if old.ID == q.ID && old.Body != q.Body {
					return Round{}, ErrInvalid
				}
			}
		}
	}
	for _, previous := range s.Rounds {
		if previous.ID == s.CurrentRound && previous.ParticipantID == participant {
			for _, old := range previous.Questions {
				resolved := slices.ContainsFunc(previous.Resolutions, func(r Resolution) bool { return r.QuestionID == old.ID })
				carried := slices.ContainsFunc(questions, func(q Question) bool { return q.ID == old.ID || q.Replaces == old.ID })
				if !resolved && !carried {
					return Round{}, ErrInvalid
				}
			}
		}
	}
	r := Round{ID: id, Supersedes: s.CurrentRound, Purpose: Clarification, ParticipantID: participant, TurnID: turn, HeadSHA: head, Questions: slices.Clone(questions)}
	s.Rounds = append(s.Rounds, r)
	return r, nil
}

// Publish also supersedes the previous round, but preserves every old question,
// response and resolution. Replies to an unconfirmed round can already be queued.
func (s *State) Publish(id string, source Source) error {
	for i := range s.Rounds {
		r := &s.Rounds[i]
		if r.ID != id {
			continue
		}
		if r.Published {
			if r.Source != nil && *r.Source == source {
				return nil
			}
			return ErrInvalid
		}
		if s.CurrentRound != "" {
			for j := range s.Rounds {
				if s.Rounds[j].ID == s.CurrentRound {
					s.Rounds[j].SupersededBy = id
				}
			}
		}
		r.Published = true
		r.Source = &source
		s.CurrentRound = id
		return nil
	}
	return ErrInvalid
}

// Accept validates authority, lifecycle and explicit target only. Free-form
// content is not interpreted as agreement or as an answer to every question.
func (s *State) Accept(id string, c Command, a Admission) Receipt {
	for _, receipt := range s.Receipts {
		if receipt.Source.Channel == a.Source.Channel && receipt.Source.ID == a.Source.ID {
			return receipt
		}
	}
	receipt := Receipt{ID: id, Source: a.Source}
	reject := func(message string) Receipt {
		receipt.Feedback = message
		s.Receipts = append(s.Receipts, receipt)
		return receipt
	}
	if !a.Authorized {
		return reject("Rejected: repository write, maintain or admin permission is required.")
	}
	if !a.Active || a.ParticipantID == "" && a.Route == "" {
		return reject("Rejected: this Work Item is not accepting input; resolve Human Handoff or lifecycle state explicitly before retrying.")
	}
	if id == "" || strings.TrimSpace(c.Body) == "" || a.Actor.ID == "" || a.Source.ID == "" || a.Source.Channel == "" || a.At.IsZero() {
		return reject("Rejected: provide nonempty content after the command.")
	}
	for _, input := range s.Inputs {
		if input.ID == id {
			return reject("Rejected: response identity already exists.")
		}
	}
	roundID := ""
	switch c.Kind {
	case "answer":
		var found *Round
		for i := range s.Rounds {
			if s.Rounds[i].ID == c.Target {
				found = &s.Rounds[i]
				break
			}
		}
		if found == nil {
			return reject("Rejected: unknown round in this Work Item; use the round ID from the current question comment.")
		}
		if found.SupersededBy != "" || found.Published && s.CurrentRound != found.ID {
			return reject(fmt.Sprintf("Rejected: round is superseded; answer current round %s.", s.CurrentRound))
		}
		if found.ParticipantID != a.ParticipantID {
			return reject("Rejected: the addressed round belongs to an obsolete Stage; use /omnigrex steer for current work.")
		}
		roundID = found.ID
	case "amend":
		var found *Input
		for i := range s.Inputs {
			if s.Inputs[i].ID == c.Target {
				found = &s.Inputs[i]
				break
			}
		}
		if found == nil {
			return reject("Rejected: unknown response in this Work Item; use the response ID from its acknowledgment.")
		}
		if found.ParticipantID != a.ParticipantID {
			return reject("Rejected: response belongs to an obsolete Stage; use /omnigrex steer for current work.")
		}
		roundID = found.RoundID
		for _, r := range s.Rounds {
			if r.ID == roundID && r.SupersededBy != "" {
				return reject(fmt.Sprintf("Rejected: round is superseded; answer current round %s.", s.CurrentRound))
			}
		}
	case "steer":
		if c.Target != "" {
			return reject("Rejected: /omnigrex steer does not take a target.")
		}
	default:
		return reject("Rejected: use /omnigrex answer <round-id>, /omnigrex amend <response-id>, or /omnigrex steer.")
	}
	input := Input{ID: id, Kind: c.Kind, RoundID: roundID, Body: c.Body, Actor: a.Actor, Source: a.Source, AcceptedAt: a.At, ParticipantID: a.ParticipantID, Route: a.Route}
	if c.Kind == "amend" {
		input.Amends = c.Target
	}
	s.Inputs = append(s.Inputs, input)
	receipt.ResponseID = id
	receipt.Feedback = fmt.Sprintf("Accepted and queued as %s. This acknowledges receipt, not interpretation or implementation authorization. Amend with /omnigrex amend %s.", id, id)
	s.Receipts = append(s.Receipts, receipt)
	return receipt
}

func (s *State) Pending(participant string) bool {
	for _, input := range s.Inputs {
		if input.ParticipantID == participant && input.DeliveredTo == "" {
			return true
		}
	}
	return false
}

// Prepare freezes all pending input at the next Turn boundary. Replays for the
// same Turn return the same batch; input arriving later is left for another Turn.
func (s *State) Prepare(participant, turn string) []Input {
	var batch []Input
	for _, input := range s.Inputs {
		if input.DeliveredTo == turn {
			batch = append(batch, input)
		}
	}
	for _, prepared := range s.Preparations {
		if prepared.TurnID == turn {
			return batch
		}
	}
	s.Preparations = append(s.Preparations, Preparation{ParticipantID: participant, TurnID: turn})
	for i := range s.Inputs {
		input := &s.Inputs[i]
		if input.ParticipantID == participant && input.DeliveredTo == "" {
			input.DeliveredTo = turn
			batch = append(batch, *input)
		}
	}
	return batch
}

// Resolve is an explicit agent interpretation backed by delivered response
// identities. Admission itself never settles questions, even for “yes to all”.
func (s *State) Resolve(roundID, turn string, resolutions []Resolution) error {
	for i := range s.Rounds {
		r := &s.Rounds[i]
		if r.ID != roundID {
			continue
		}
		if !r.Published || r.SupersededBy != "" {
			return ErrInvalid
		}
		if !slices.ContainsFunc(s.Preparations, func(p Preparation) bool { return p.TurnID == turn && p.ParticipantID == r.ParticipantID }) {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, resolution := range resolutions {
			if seen[resolution.QuestionID] {
				return ErrInvalid
			}
			seen[resolution.QuestionID] = true
			if strings.TrimSpace(resolution.Explanation) == "" || len(resolution.ResponseIDs) == 0 && !resolution.Obsolete {
				return ErrInvalid
			}
			if !slices.ContainsFunc(r.Questions, func(q Question) bool { return q.ID == resolution.QuestionID }) {
				return ErrInvalid
			}
			if slices.ContainsFunc(r.Resolutions, func(old Resolution) bool { return old.QuestionID == resolution.QuestionID }) {
				return ErrInvalid
			}
			for _, id := range resolution.ResponseIDs {
				if !slices.ContainsFunc(s.Inputs, func(in Input) bool {
					return in.ID == id && in.DeliveredTo != "" && in.ParticipantID == r.ParticipantID && (in.RoundID == roundID || in.RoundID == "")
				}) {
					return ErrInvalid
				}
			}
		}
		for _, resolution := range resolutions {
			resolution.ResponseIDs = slices.Clone(resolution.ResponseIDs)
			r.Resolutions = append(r.Resolutions, resolution)
		}
		return nil
	}
	return ErrInvalid
}

// Unresolved fences completion without interpreting natural-language answers.
func (s *State) Unresolved(participant string) bool {
	for _, round := range s.Rounds {
		if round.ID == s.CurrentRound && round.ParticipantID == participant {
			for _, question := range round.Questions {
				if !slices.ContainsFunc(round.Resolutions, func(r Resolution) bool { return r.QuestionID == question.ID }) {
					return true
				}
			}
		}
	}
	return false
}
