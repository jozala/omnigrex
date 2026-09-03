package opencode_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/opencode"
)

func TestSessionConfigurationAppliesAndVerifiesOptionsInOrder(t *testing.T) {
	initial := configOptions("other/model", "low", "build")
	setter := &recordingSetter{responses: [][]json.RawMessage{
		configOptions("provider/model", "low", "build"),
		configOptions("provider/model", "high", "build"),
		configOptions("provider/model", "high", "omnigrex-reviewer"),
	}}
	configuration := opencode.SessionConfiguration{
		Model:   "provider/model",
		Variant: "high",
		Mode:    "omnigrex-reviewer",
	}

	if err := configuration.Apply(context.Background(), setter, "session-1", initial); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	want := []configCall{
		{SessionID: "session-1", ConfigID: "model", Value: "provider/model"},
		{SessionID: "session-1", ConfigID: "effort", Value: "high"},
		{SessionID: "session-1", ConfigID: "mode", Value: "omnigrex-reviewer"},
	}
	if !reflect.DeepEqual(setter.calls, want) {
		t.Fatalf("SetConfigOption calls = %#v, want %#v", setter.calls, want)
	}
}

func TestSessionConfigurationFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		initial   []json.RawMessage
		responses [][]json.RawMessage
		want      error
		wantCalls int
	}{
		{
			name:    "missing requested option",
			initial: []json.RawMessage{json.RawMessage(`{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"}]}`)},
			want:    opencode.ErrConfigOptionUnsupported,
		},
		{
			name:    "unsupported requested value",
			initial: []json.RawMessage{json.RawMessage(`{"id":"model","type":"select","currentValue":"other/model","options":[{"value":"other/model"}]}`)},
			want:    opencode.ErrConfigOptionUnsupported,
		},
		{
			name:      "malformed returned options",
			initial:   configOptions("other/model", "low", "build"),
			responses: [][]json.RawMessage{{json.RawMessage(`{"id":"model","type":"select","currentValue":3,"options":[]}`)}},
			want:      opencode.ErrConfigOptionMalformed,
			wantCalls: 1,
		},
		{
			name:      "returned selection mismatch",
			initial:   configOptions("other/model", "low", "build"),
			responses: [][]json.RawMessage{configOptions("other/model", "low", "build")},
			want:      opencode.ErrConfigSelectionMismatch,
			wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setter := &recordingSetter{responses: test.responses}
			err := (opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-reviewer"}).Apply(
				context.Background(), setter, "session-1", test.initial,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("Apply() error = %v, want %v", err, test.want)
			}
			if len(setter.calls) != test.wantCalls {
				t.Fatalf("SetConfigOption calls = %d, want %d", len(setter.calls), test.wantCalls)
			}
		})
	}
}

func TestSessionConfigurationWithoutVariantAppliesModelThenMode(t *testing.T) {
	setter := &recordingSetter{responses: [][]json.RawMessage{
		configOptions("provider/model", "low", "build"),
		configOptions("provider/model", "low", "omnigrex-developer"),
	}}
	configuration := opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"}
	if err := configuration.Apply(context.Background(), setter, "session-1", configOptions("other/model", "high", "build")); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := []string{setter.calls[0].ConfigID, setter.calls[1].ConfigID}; !reflect.DeepEqual(got, []string{"model", "mode"}) {
		t.Fatalf("option order = %v, want model then mode", got)
	}
}

func TestSessionConfigurationRejectsLaterSetterResettingEarlierSelection(t *testing.T) {
	tests := []struct {
		name  string
		final []json.RawMessage
	}{
		{name: "model", final: configOptions("other/model", "high", "omnigrex-reviewer")},
		{name: "variant", final: configOptions("provider/model", "low", "omnigrex-reviewer")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setter := &recordingSetter{responses: [][]json.RawMessage{
				configOptions("provider/model", "low", "build"),
				configOptions("provider/model", "high", "build"),
				test.final,
			}}
			configuration := opencode.SessionConfiguration{
				Model: "provider/model", Variant: "high", Mode: "omnigrex-reviewer",
			}

			err := configuration.Apply(context.Background(), setter, "session-1", configOptions("other/model", "low", "build"))
			if !errors.Is(err, opencode.ErrConfigSelectionMismatch) {
				t.Fatalf("Apply() error = %v, want ErrConfigSelectionMismatch", err)
			}
		})
	}
}

type configCall struct {
	SessionID string
	ConfigID  string
	Value     any
}

type recordingSetter struct {
	calls     []configCall
	responses [][]json.RawMessage
}

func (setter *recordingSetter) SetConfigOption(_ context.Context, sessionID, configID string, value any) ([]json.RawMessage, error) {
	setter.calls = append(setter.calls, configCall{SessionID: sessionID, ConfigID: configID, Value: value})
	response := setter.responses[0]
	setter.responses = setter.responses[1:]
	return response, nil
}

func configOptions(model, effort, mode string) []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"id":"model","type":"select","currentValue":"` + model + `","options":[{"value":"other/model"},{"value":"provider/model"}]}`),
		json.RawMessage(`{"id":"effort","type":"select","currentValue":"` + effort + `","options":[{"value":"low"},{"value":"high"}]}`),
		json.RawMessage(`{"id":"mode","type":"select","currentValue":"` + mode + `","options":[{"value":"build"},{"value":"omnigrex-developer"},{"value":"omnigrex-reviewer"}]}`),
	}
}
