package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrConfigOptionMalformed   = errors.New("malformed ACP session configuration options")
	ErrConfigOptionUnsupported = errors.New("unsupported ACP session configuration option")
	ErrConfigSelectionMismatch = errors.New("ACP session configuration selection mismatch")
)

type SetConfigOption interface {
	SetConfigOption(context.Context, string, string, any) ([]json.RawMessage, error)
}

func (configuration SessionConfiguration) Apply(
	ctx context.Context,
	setter SetConfigOption,
	sessionID string,
	available []json.RawMessage,
) error {
	if setter == nil || sessionID == "" || configuration.Model == "" || configuration.Mode == "" {
		return ErrConfigOptionMalformed
	}

	desired := []struct {
		id    string
		value string
	}{
		{id: "model", value: configuration.Model},
	}
	if configuration.Variant != "" {
		desired = append(desired, struct {
			id    string
			value string
		}{id: "effort", value: configuration.Variant})
	}
	desired = append(desired, struct {
		id    string
		value string
	}{id: "mode", value: configuration.Mode})

	options, err := parseConfigOptions(available)
	if err != nil {
		return err
	}
	for _, selection := range desired {
		if err := requireSupported(options, selection.id, selection.value); err != nil {
			return err
		}
		returned, err := setter.SetConfigOption(ctx, sessionID, selection.id, selection.value)
		if err != nil {
			return fmt.Errorf("set OpenCode session %s option: %w", selection.id, err)
		}
		options, err = parseConfigOptions(returned)
		if err != nil {
			return err
		}
		option, ok := options[selection.id]
		if !ok || !option.supports(selection.value) {
			return fmt.Errorf("%w: %s", ErrConfigOptionUnsupported, selection.id)
		}
		if option.current != selection.value {
			return fmt.Errorf("%w: %s", ErrConfigSelectionMismatch, selection.id)
		}
	}
	for _, selection := range desired {
		if err := requireSupported(options, selection.id, selection.value); err != nil {
			return err
		}
		if options[selection.id].current != selection.value {
			return fmt.Errorf("%w: %s", ErrConfigSelectionMismatch, selection.id)
		}
	}
	return nil
}

type configOption struct {
	current string
	values  map[string]struct{}
}

func (option configOption) supports(value string) bool {
	_, ok := option.values[value]
	return ok
}

func parseConfigOptions(raw []json.RawMessage) (map[string]configOption, error) {
	if len(raw) == 0 {
		return nil, ErrConfigOptionMalformed
	}
	parsed := make(map[string]configOption, len(raw))
	for _, item := range raw {
		var wire struct {
			ID           string  `json:"id"`
			Type         string  `json:"type"`
			CurrentValue *string `json:"currentValue"`
			Options      []struct {
				Value *string `json:"value"`
			} `json:"options"`
		}
		if err := json.Unmarshal(item, &wire); err != nil || wire.ID == "" || wire.Type != "select" || wire.CurrentValue == nil || len(wire.Options) == 0 {
			return nil, ErrConfigOptionMalformed
		}
		if _, duplicate := parsed[wire.ID]; duplicate {
			return nil, ErrConfigOptionMalformed
		}
		option := configOption{current: *wire.CurrentValue, values: make(map[string]struct{}, len(wire.Options))}
		for _, candidate := range wire.Options {
			if candidate.Value == nil || *candidate.Value == "" {
				return nil, ErrConfigOptionMalformed
			}
			if _, duplicate := option.values[*candidate.Value]; duplicate {
				return nil, ErrConfigOptionMalformed
			}
			option.values[*candidate.Value] = struct{}{}
		}
		parsed[wire.ID] = option
	}
	return parsed, nil
}

func requireSupported(options map[string]configOption, id, value string) error {
	option, ok := options[id]
	if !ok || !option.supports(value) {
		return fmt.Errorf("%w: %s", ErrConfigOptionUnsupported, id)
	}
	return nil
}
