package piresources

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// selectedBundle is deliberately checked in, never generated during a build.
//
//go:embed selected-bundle.json
var selectedBundle []byte

// Bundle identifies the sole bundle admitted for fresh production Participants.
// Qualification records the original exact-pair smokes, not the current build.
type Bundle struct {
	Version       int                 `json:"version"`
	Platform      string              `json:"platform"`
	Image         string              `json:"image"`
	SourceCommit  string              `json:"source_commit"`
	Qualification BundleQualification `json:"qualification"`
	Resources     json.RawMessage     `json:"resources"`
}
type BundleQualification struct {
	RunID             int64 `json:"run_id"`
	Attempt           int   `json:"attempt"`
	DockerJobID       int64 `json:"docker_job_id"`
	MicrosandboxJobID int64 `json:"microsandbox_job_id"`
}

// SelectedBundle never consults checkout resources or discovers images. Historical
// contracts are decoded independently and do not require an entry here.
func SelectedBundle(os, arch string) (Bundle, error) {
	b, err := ParseBundle(selectedBundle)
	if err != nil {
		return Bundle{}, err
	}
	if os+"/"+arch != b.Platform {
		return Bundle{}, fmt.Errorf("no selected production Pi bundle for %s/%s; use linux/amd64 for new Participants; arm64 local/candidate testing and retained Participants are separate", os, arch)
	}
	return b, nil
}

// ParseBundle validates a captured selection, including older release provenance.
func ParseBundle(data []byte) (Bundle, error) {
	var b Bundle
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if len(data) > 128<<10 || d.Decode(&b) != nil || d.Decode(new(any)) != io.EOF {
		return Bundle{}, fmt.Errorf("invalid selected Pi bundle record")
	}
	q := b.Qualification
	if b.Version != 1 || b.Platform != "linux/amd64" || !regexp.MustCompile(`^ghcr.io/jozala/omnigrex/pi@sha256:[0-9a-f]{64}$`).MatchString(b.Image) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(b.SourceCommit) || q.RunID <= 0 || q.Attempt <= 0 || q.DockerJobID <= 0 || q.MicrosandboxJobID <= 0 || q.DockerJobID == q.MicrosandboxJobID {
		return Bundle{}, fmt.Errorf("incomplete selected Pi bundle identity, provenance or bootstrap requirements")
	}
	resources, err := NormalizeContract(b.Resources)
	if err != nil {
		return Bundle{}, fmt.Errorf("selected Pi bundle: %w", err)
	}
	var inventory ResourceContract
	_ = json.Unmarshal(resources, &inventory)
	if inventory.Version != 2 {
		return Bundle{}, fmt.Errorf("selected Pi bundle requires complete bootstrap/executable inventory")
	}
	b.Resources = resources
	return b, nil
}

// UnmarshalJSON canonicalizes resource JSON so release identity comparisons do
// not depend on indentation or JSONB key order. Unknown fields still fail closed.
func (b *Bundle) UnmarshalJSON(data []byte) error {
	type plain Bundle
	var value plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	resources, err := NormalizeContract(value.Resources)
	if err != nil {
		return err
	}
	value.Resources = resources
	*b = Bundle(value)
	return nil
}
