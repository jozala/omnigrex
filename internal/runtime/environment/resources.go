package environment

import (
	"encoding/json"
	"fmt"
	piresources "github.com/jozala/omnigrex/agent/pi"
)

// QualificationScript is platform policy, not image-provided ancestry metadata.
// Run it in a bounded, non-networked helper before any repository scripts.
func QualificationScript(architecture, runtime string, resourceContracts ...json.RawMessage) (string, error) {
	var opencode, mise string
	switch architecture {
	case "amd64":
		opencode = "294bfa0f4647e0fc5d3edceed94e8dc1047757975689f4269aadb91d6774f9ed"
		mise = "2af841603ad2981529f230add15c601c64e3a81a79f4f41e9860eb38b7672771"
	case "arm64":
		opencode = "9b9044ede6449dc34888b85f0a51be5777a5886fe5e1cbd67898b13f8a5bec04"
		mise = "a2f1321f1189f2e1e62b1bd0d20fc59ba3c6a0b38b2c64fcfd29296ca44d2905"
	default:
		return "", fmt.Errorf("unsupported golden-image architecture %q", architecture)
	}
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
	if runtime != "opencode-acp" {
		return "", fmt.Errorf("unsupported golden-image runtime %q", runtime)
	}
	// The release checksum for OpenCode covers its compressed distribution.
	// The image retains that archive read-only so verification can reconstruct
	// and compare the executable rather than accepting a version claim.
	return fmt.Sprintf(`
test -x /usr/local/bin/omnigrex-environment
test "$(/usr/local/bin/omnigrex-environment contract-version)" = omnigrex-environment/v1
printf '%%s  %%s\n' '%s' /opt/omnigrex/opencode.tar.gz '%s' /usr/local/bin/mise | sha256sum -c -
test "$(tar -xzOf /opt/omnigrex/opencode.tar.gz opencode | sha256sum | cut -d ' ' -f 1)" = "$(sha256sum /usr/local/bin/opencode | cut -d ' ' -f 1)"
test -x /usr/local/bin/opencode
test -x /usr/local/bin/mise
test -x /usr/bin/git
`, opencode, mise), nil
}
