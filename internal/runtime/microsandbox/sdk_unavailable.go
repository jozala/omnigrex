//go:build !microsandbox || !cgo

package microsandbox

import (
	"fmt"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func newPlatform(Options) (platform, error) {
	return nil, fmt.Errorf("%w: this binary does not include the Microsandbox Go SDK (requires CGO and the microsandbox build tag)", execution.ErrUnsupportedRequirement)
}
