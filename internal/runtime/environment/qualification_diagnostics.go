package environment

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	piresources "github.com/jozala/omnigrex/agent/pi"
)

// QualificationOutput accepts only complete, exact failure labels from the
// host-generated script. It never retains arbitrary guest output. This is a
// per-call diagnostic, not a guest compatibility protocol or success evidence.
// Overlong, truncated, conflicting or excessive output invalidates the detail.
type QualificationOutput struct {
	allowed  map[string]bool
	line     []byte
	total    int
	invalid  bool
	observed bool
	check    string
}

const qualificationOutputLimit = 16 << 10
const qualificationPrefix = "Pi resource check failed: "

func NewQualificationOutput(architecture, runtime string, resources json.RawMessage) *QualificationOutput {
	output := &QualificationOutput{allowed: map[string]bool{}}
	script, err := QualificationScript(architecture, runtime, resources)
	if err != nil {
		return output
	}
	for _, line := range strings.Split(script, "\n") {
		if label, ok := strings.CutPrefix(line, "omnigrex_check='"); ok {
			if label, ok = strings.CutSuffix(label, "'"); ok && label != "unknown stage" {
				output.allowed[qualificationPrefix+label] = true
			}
		}
	}
	return output
}

func (output *QualificationOutput) Write(data []byte) (int, error) {
	n := len(data)
	for _, b := range data {
		output.total++
		if output.invalid {
			continue
		}
		if output.total > qualificationOutputLimit {
			output.invalid = true
			output.line = nil
			output.check = ""
			continue
		}
		if b == '\n' {
			line := string(output.line)
			if output.allowed[line] {
				label := strings.TrimPrefix(line, qualificationPrefix)
				if output.check != "" && output.check != label {
					output.invalid = true
					output.check = ""
				} else {
					output.check = label
				}
			} else if strings.HasPrefix(line, "Pi resource check failed:") {
				// An unexpected diagnostic-looking line makes the capture
				// ambiguous, even if another complete label looked valid.
				output.Invalidate()
			}
			output.line = output.line[:0]
		} else if len(output.line) < 256 {
			output.line = append(output.line, b)
		} else {
			output.invalid = true
			output.line = nil
			output.check = ""
		}
	}
	return n, nil
}

// Invalidate discards detail when capture or transport framing is incomplete.
func (output *QualificationOutput) Invalidate() {
	output.invalid = true
	output.line = nil
	output.check = ""
}

// ObserveNonzeroExit gates detail on actual exit evidence, never a lost exec.
func (output *QualificationOutput) ObserveNonzeroExit() { output.observed = true }

func (output *QualificationOutput) Check() string {
	if output == nil || !output.observed || output.invalid || len(output.line) != 0 || output.check == "" {
		return "unknown stage (diagnostic unavailable or untrusted)"
	}
	return output.check + " (guest-reported failed check)"
}

// QualificationFailure adds host-owned binding context without reclassifying
// execution, cancellation, availability, native-release, or cleanup failures.
// Like decision 0034, returned errors stay sanitized; raw SDK evidence retains
// its existing restricted operator-log boundary.
func QualificationFailure(image, os, arch string, resources json.RawMessage, output *QualificationOutput, err error) error {
	if err == nil {
		return nil
	}
	identity := "missing/invalid bound inventory"
	if canonical, normalizeErr := piresources.NormalizeContract(resources); normalizeErr == nil {
		identity = fmt.Sprintf("sha256:%x", sha256.Sum256(canonical))
	}
	return fmt.Errorf("runtime resource qualification: bound image=%q platform=%q inventory=%s; %s; require the original bound image and canonical inventory for retained Participants (do not rebind): %w", image, os+"/"+arch, identity, output.Check(), err)
}
