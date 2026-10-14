package environment

import (
	"encoding/json"
	"fmt"
	piresources "github.com/jozala/omnigrex/agent/pi"
)

// QualificationScript is platform policy, not image-provided ancestry metadata.
// Run it in a bounded, non-networked helper before any repository scripts.
func QualificationScript(architecture, runtime string, resourceContracts ...json.RawMessage) (string, error) {
	if runtime == "pi-rpc" {
		var content json.RawMessage
		if len(resourceContracts) > 1 {
			return "", fmt.Errorf("multiple golden resource contracts")
		}
		if len(resourceContracts) == 1 {
			content = resourceContracts[0]
		}
		script, err := piresources.QualificationScript(architecture, content)
		if err != nil {
			return "", err
		}
		return `
test -x /usr/local/bin/omnigrex-environment
test "$(/usr/local/bin/omnigrex-environment contract-version)" = omnigrex-environment/v1
test -x /usr/bin/git
` + script, nil
	}
	return "", fmt.Errorf("unsupported golden-image runtime %q", runtime)
}
