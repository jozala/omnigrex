package reviewpolicy

import (
	"testing"
	"time"
)

func TestPolicyDefaultsAndExplicitSelection(t *testing.T) {
	p, err := Parse([]byte("version: 1\n"))
	if err != nil || p.Start != "after_ci" || p.Timeout != 30*time.Minute || p.RegistrationGrace != time.Minute {
		t.Fatalf("default = %+v, %v", p, err)
	}
	p, err = Parse([]byte("version: 1\nreview:\n  start: immediate\n  ci:\n    selection: explicit\n    timeout: 2h\n    registration-grace: 0s\n    checks:\n      - context: build\n        source: check\n        app-id: 99\n"))
	if err != nil || p.Start != "immediate" || len(p.Checks) != 1 || p.Checks[0].AppID != 99 {
		t.Fatalf("explicit = %+v, %v", p, err)
	}
}
func TestInvalidPolicyCannotBypassCI(t *testing.T) {
	for _, content := range []string{"version: 1\nreview: null", "version: 1\nreview:\n  start: \"\"", "version: 1\nreview:\n  ci:\n    checks: []", "version: 1\nreview:\n  ci:\n    selection: explicit\n    checks:\n      - context: build\n        source: check\n        app-id: 0", "", "version: 2", "version: 1\nunknown: true", "version: 1\n---\nversion: 1", "version: 1\nreview:\n  start: never", "version: 1\nreview:\n  ci:\n    selection: explicit", "version: 1\nreview:\n  ci:\n    selection: explicit\n    checks: []", "version: 1\nreview:\n  ci:\n    timeout: 0s", "version: 1\nreview:\n  ci:\n    timeout: 121m", "version: 1\nreview:\n  ci:\n    registration-grace: -1s", "version: 1\nreview:\n  ci:\n    registration-grace: 301s", "version: 1\nreview:\n  ci:\n    timeout: 1s\n    registration-grace: 2s", "version: 1\nreview:\n  ci:\n    timeout: 1.5s", "version: 1\nreview:\n  ci:\n    selection: explicit\n    checks:\n      - context: build\n        source: status\n        app-id: 4", "version: 1\nreview:\n  ci:\n    checks:\n      - context: build\n        source: check"} {
		t.Run(content, func(t *testing.T) {
			if p, err := Parse([]byte(content)); err == nil {
				t.Fatalf("invalid policy admitted: %+v", p)
			}
		})
	}
}
