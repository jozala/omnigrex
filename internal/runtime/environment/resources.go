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
		} else {
			// Omission requests a default-selection diagnostic. An explicitly
			// empty retained inventory must never be reconstructed.
			bundle, err := piresources.SelectedBundle("linux", architecture)
			if err != nil {
				return "", err
			}
			content = bundle.Resources
		}
		script, err := piresources.QualificationScript(architecture, content)
		if err != nil {
			return "", err
		}
		return `
set -eu
omnigrex_check='unknown stage'
trap 'status=$?; if [ "$status" -ne 0 ]; then printf "Pi resource check failed: %s\n" "$omnigrex_check" >&2; fi' 0
omnigrex_check='executable /usr/local/bin/omnigrex-environment'
test -x /usr/local/bin/omnigrex-environment
omnigrex_check='bootstrap identity'
test "$(/usr/local/bin/omnigrex-environment contract-version)" = omnigrex-environment/v1
omnigrex_check='executable /usr/bin/git'
test -x /usr/bin/git
` + script + "\ntrap - 0\n", nil
	}
	return "", fmt.Errorf("unsupported golden-image runtime %q", runtime)
}
