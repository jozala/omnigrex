package github

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// CIEvidencePage extends get_check_runs without changing its head identity.
// Collections share one bounded continuation, but each is independently sliced;
// empty arrays on a later page do not mean the full inventory is empty.
type CIEvidencePage struct {
	HeadSHA    string             `json:"head_sha"`
	CheckRuns  []CheckRun         `json:"check_runs"`
	Mapping    CITargetMapping    `json:"target_mapping"`
	Inventory  CIInventory        `json:"required_policy"`
	Targets    []CITargetEvidence `json:"targets"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor,omitempty"`
}
type CITargetEvidence struct {
	Observation CIObservation         `json:"observation"`
	Required    []CIRequirementResult `json:"required_results"`
}
type evidenceCursor struct {
	RepositoryID int64  `json:"repository_id"`
	Head         string `json:"head"`
	Fingerprint  string `json:"fingerprint"`
	Offset       int    `json:"offset"`
}

func evidenceWindow[T any](items []T, start, end int) []T {
	if start >= len(items) {
		return []T{}
	}
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

func PageCIEvidence(inventory CIInventory, observations []CIObservation, mapping CITargetMapping, cursor string) (CIEvidencePage, error) {
	// Exclude only observation-clock metadata; identity, actual results, policy,
	// producer, attempt and failure changes invalidate continuation.
	stableInventory := inventory
	stableInventory.ObservedAt = time.Time{}
	stableMapping := mapping
	stableMapping.ObservedAt = time.Time{}
	stableObservations := append([]CIObservation(nil), observations...)
	for i := range stableObservations {
		stableObservations[i].ObservedAt = time.Time{}
	}
	stable, _ := json.Marshal(struct {
		Inventory    CIInventory
		Mapping      CITargetMapping
		Observations []CIObservation
	}{stableInventory, stableMapping, stableObservations})
	digest := sha256.Sum256(stable)
	fingerprint := hex.EncodeToString(digest[:])
	offset := 0
	if len(cursor) > 4096 {
		return CIEvidencePage{}, &ConfigurationError{Cause: fmt.Errorf("continuation is invalid")}
	}
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(decoded) > 2048 {
			return CIEvidencePage{}, &ConfigurationError{Cause: fmt.Errorf("continuation is invalid")}
		}
		var prior evidenceCursor
		if json.Unmarshal(decoded, &prior) != nil || prior.RepositoryID != mapping.RepositoryID || prior.Head != mapping.ReviewedHead || prior.Fingerprint != fingerprint || prior.Offset <= 0 {
			return CIEvidencePage{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
		}
		offset = prior.Offset
	}
	targets := make([]CITargetEvidence, 0, len(observations))
	longest := max(len(inventory.Requirements), len(inventory.Unresolved), len(inventory.Failures))
	for _, observed := range observations {
		matched := MatchRequiredCI(inventory, observed)
		targets = append(targets, CITargetEvidence{Observation: observed, Required: matched})
		longest = max(longest, len(observed.Checks), len(observed.Statuses), len(observed.Failures), len(matched))
	}
	if offset > 0 && offset >= longest {
		return CIEvidencePage{}, &ConfigurationError{Cause: fmt.Errorf("stale continuation")}
	}
	for count := 20; count >= 1; count-- {
		end := offset + count
		page := CIEvidencePage{HeadSHA: mapping.ReviewedHead, Mapping: mapping, Inventory: inventory, Targets: make([]CITargetEvidence, 0, len(targets)), CheckRuns: []CheckRun{}, HasMore: end < longest}
		page.Inventory.Requirements = evidenceWindow(inventory.Requirements, offset, end)
		page.Inventory.Unresolved = evidenceWindow(inventory.Unresolved, offset, end)
		page.Inventory.Failures = evidenceWindow(inventory.Failures, offset, end)
		for _, target := range targets {
			observed := target.Observation
			observed.Checks = evidenceWindow(observed.Checks, offset, end)
			observed.Statuses = evidenceWindow(observed.Statuses, offset, end)
			observed.Failures = evidenceWindow(observed.Failures, offset, end)
			page.Targets = append(page.Targets, CITargetEvidence{Observation: observed, Required: evidenceWindow(target.Required, offset, end)})
			if observed.HeadSHA == mapping.ReviewedHead {
				page.CheckRuns = observed.Checks
			}
		}
		if page.HasMore {
			raw, _ := json.Marshal(evidenceCursor{RepositoryID: mapping.RepositoryID, Head: mapping.ReviewedHead, Fingerprint: fingerprint, Offset: end})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		if err := ensureCISerializedBound(page); err == nil {
			return page, nil
		}
	}
	return CIEvidencePage{}, &ConfigurationError{Cause: fmt.Errorf("evidence response exceeds size limit")}
}
