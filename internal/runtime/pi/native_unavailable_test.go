//go:build integration && (!microsandbox || !cgo)

package pi_test

import (
	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"testing"
)

func nativeDiagnosticFixture(t *testing.T) (string, execution.Platform, config.Config) {
	t.Helper()
	t.Fatal("Microsandbox Pi gate requires the native SDK build; run mise run test-pi-microsandbox-integration")
	return "", execution.Platform{}, config.Config{}
}

func nativeSessionFactory(t *testing.T, _ config.Config) (execution.Factory, func(string)) {
	t.Helper()
	t.Fatal("Microsandbox Session gate requires native SDK build")
	return nil, nil
}
