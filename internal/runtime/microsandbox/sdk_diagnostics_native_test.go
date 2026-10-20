//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func TestSDKErrorLogsDetailsButReturnsSafeError(t *testing.T) {
	output := captureDiagnostics(t)
	source := errors.New("original SDK cause fake-api-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sdkError(ctx, sdkCall("Kill", metadata{ID: "native-id"}), source)
	if !strings.Contains(output.String(), source.Error()) || !strings.Contains(output.String(), "Kill") {
		t.Fatal(output.String())
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, source) || strings.Contains(err.Error(), "api-key") {
		t.Fatal(err)
	}
}

func TestSDKExpectedAbsenceDoesNotLogFailure(t *testing.T) {
	output := captureDiagnostics(t)
	source := &sdk.Error{Kind: sdk.ErrSandboxNotFound}
	if !errors.Is(sdkLookupError(context.Background(), sdkCall("GetSandbox", metadata{}), source), execution.ErrNotFound) || output.Len() != 0 {
		t.Fatal(output.String())
	}
}
