//go:build integration && (!microsandbox || !cgo)

package store_test

import (
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"os"
	"testing"
)

func workflowQualificationImage(t *testing.T) (string, profile.Platform) {
	t.Helper()
	return os.Getenv("OMNIGREX_PI_WORKFLOW_IMAGE"), profile.Platform{OS: "linux", Arch: os.Getenv("OMNIGREX_PI_WORKFLOW_ARCH")}
}
