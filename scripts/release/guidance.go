package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
)

// guidance is derived output, not a release asset or another selection record.
// verify binds the manifest to the packaged build.json, including Pi selection.
func guidance(dir, developmentFile string) ([]byte, error) {
	m, _, err := verify(dir)
	if err != nil {
		return nil, err
	}
	if len(m.Qualification) != len(requiredJobs) {
		return nil, errors.New("complete readiness evidence required for deployment guidance")
	}
	for _, name := range requiredJobs {
		if m.Qualification[name] <= 0 {
			return nil, errors.New("complete readiness evidence required for deployment guidance")
		}
	}
	dev, err := os.ReadFile(developmentFile)
	if err != nil {
		return nil, err
	}
	// Accept only the existing qualified artifact's document shape. Do not render
	// arbitrary YAML, secrets, or Markdown supplied alongside a product payload.
	if !regexp.MustCompile(`\Aversion: 1\nimage: ghcr.io/jozala/omnigrex/development@sha256:[0-9a-f]{64}\nplatform: linux/amd64\nrequirements:\n  - hardware-isolation\n  - nested-containers\n  - tool-installation\n\z`).Match(dev) {
		return nil, errors.New("invalid qualified development-environment document")
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "## Qualified main CI candidate — deployment configuration\n\nSource: `%s`, CI run %d, attempt %d. Required exact-digest qualifications and release readiness succeeded.\n\nThis is a qualified CI candidate, **not a manually designated public alpha/stable release**. Nothing is deployed or adopted automatically. For normal installation, select a designated GitHub Release and use its source run/attempt, not a newer CI run.\n\nCopy only the three values for your chosen backend into your existing private `.env`. These non-secret snippets do not replace your full configuration or credentials. Use the matching packaged Compose overlay.\n", m.Commit, m.RunID, m.Attempt)
	for _, backend := range []string{"docker", "microsandbox"} {
		product := "orchestrator"
		if backend == "microsandbox" {
			product += "-microsandbox"
		}
		fmt.Fprintf(&out, "\n### %s\n\n```dotenv\nOMNIGREX_ORCHESTRATOR_IMAGE=%s\nOMNIGREX_PI_IMAGE=%s\nOMNIGREX_PI_PLATFORM=%s\n```\n", backend, m.Images[product], m.Images["pi"], m.Platform)
	}
	fmt.Fprint(&out, "\n### Qualified Omnigrex repository development environment\n\nThe existing `development-environment` artifact passed exact-digest Microsandbox qualification. It is specific to the Omnigrex repository, independently adopted as `.omnigrex/environment.yaml` after provisioning. It is **not an automatic update for every managed repository**, a product deployment requirement, or a change to retained Participant bindings.\n\n```yaml\n")
	out.Write(dev)
	fmt.Fprint(&out, "```\n")
	return out.Bytes(), nil
}
