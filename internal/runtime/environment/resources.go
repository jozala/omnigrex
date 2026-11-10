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
trap 'status=$?; if [ "$status" -ne 0 ]; then printf "%s\n" "Pi resource qualification failed: image must contain the exact bound runtime inventory before repository setup; for new Participants build and qualify a self-contained image against agent/pi/selected-bundle.json, then explicitly provision and promote its digest; retained Participants require their original image. No runtime is installed automatically." >&2; fi' 0
test -x /usr/local/bin/omnigrex-environment
test "$(/usr/local/bin/omnigrex-environment contract-version)" = omnigrex-environment/v1
test -x /usr/bin/git
` + script + "\ntrap - 0\n", nil
	}
	return "", fmt.Errorf("unsupported golden-image runtime %q", runtime)
}
