package piresources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// CandidateBundle describes exact contents to test, not authority for production
// admission. It deliberately has no successful-qualification claims.
type CandidateBundle struct {
	Version      int             `json:"version"`
	Platform     string          `json:"platform"`
	Image        string          `json:"image"`
	SourceCommit string          `json:"source_commit"`
	Resources    json.RawMessage `json:"resources"`
}

func ParseCandidate(data []byte) (CandidateBundle, error) {
	var c CandidateBundle
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if len(data) > 128<<10 || d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.Platform != "linux/amd64" || !regexp.MustCompile(`^ghcr.io/jozala/omnigrex/pi@sha256:[0-9a-f]{64}$`).MatchString(c.Image) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.SourceCommit) {
		return c, fmt.Errorf("invalid Pi candidate identity")
	}
	resources, err := NormalizeContract(c.Resources)
	if err != nil {
		return c, err
	}
	var inventory ResourceContract
	_ = json.Unmarshal(resources, &inventory)
	if inventory.Version != 2 {
		return c, fmt.Errorf("candidate requires complete inventory")
	}
	c.Resources = resources
	return c, nil
}

func (b Bundle) Candidate() CandidateBundle {
	return CandidateBundle{Version: 1, Platform: b.Platform, Image: b.Image, SourceCommit: b.SourceCommit, Resources: b.Resources}
}
