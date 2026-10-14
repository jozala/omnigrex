// Package session coordinates durable Agent Session control and fenced preparation.
package session

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrCapabilityMismatch          = errors.New("agent capabilities do not match durable Session")
	ErrHumanPromptIdentityMismatch = errors.New("human prompt does not match durable Session control")
	ErrHumanPromptOutcomeUncertain = errors.New("human prompt outcome is uncertain")
	ErrStatePathMismatch           = errors.New("retained state does not match Agent Session")
	ErrTurnIdentityMismatch        = errors.New("Agent Turn lease does not match durable Session")
)

type Disposition string

const (
	Created                             Disposition = "created"
	Recovered                           Disposition = "recovered"
	Continued                           Disposition = "continued"
	defaultHumanPromptLeaseDuration                 = 30 * time.Second
	defaultHumanPromptHeartbeatInterval             = 10 * time.Second
	humanPromptReleaseTimeout                       = 5 * time.Second
)

func humanPromptTiming(request RuntimeHumanPromptRequest) (time.Duration, time.Duration, error) {
	duration, interval := request.LeaseDuration, request.HeartbeatInterval
	if duration == 0 {
		duration = defaultHumanPromptLeaseDuration
	}
	if interval == 0 {
		interval = defaultHumanPromptHeartbeatInterval
	}
	const maximum = 365 * 24 * time.Hour
	if duration < time.Microsecond || duration > maximum || interval < time.Microsecond || interval > maximum || interval >= duration {
		return 0, 0, errors.New("human prompt heartbeat must be positive and shorter than its lease")
	}
	return duration, interval, nil
}

func canonicalJSON(value json.RawMessage) (json.RawMessage, error) {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
