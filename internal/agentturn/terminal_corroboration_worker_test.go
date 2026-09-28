package agentturn

import (
	"testing"
	"time"
)

func TestTerminalCorroborationBackoffIsBounded(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 5 * time.Second}, {2, 10 * time.Second},
		{3, 20 * time.Second}, {6, 2 * time.Minute},
		{1000000, 2 * time.Minute},
	} {
		if got := terminalCorroborationBackoff(test.attempt); got != test.want {
			t.Errorf("attempt %d backoff = %s, want %s", test.attempt, got, test.want)
		}
	}
}
