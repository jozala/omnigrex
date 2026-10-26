// Package publication defines durable publication evidence shared by the MCP
// producers and the store. It contains no execution or Workflow authority.
package publication

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// OpenResult is the complete, bounded evidence for a successful scoped open_pr.
// HeadSHA is the operation's opening head, never a later observed branch tip.
// Version 1 requires a quiescent upgrade; incomplete pre-versioned results are
// deliberately not accepted as association evidence.
type OpenResult struct {
	Version         int    `json:"version"`
	PullRequestID   int64  `json:"pull_request_id"`
	NodeID          string `json:"node_id"`
	Number          int64  `json:"number"`
	HTMLURL         string `json:"html_url"`
	HeadSHA         string `json:"head_sha"`
	RepositoryID    int64  `json:"repository_id"`
	RepositoryOwner string `json:"repository_owner"`
	RepositoryName  string `json:"repository_name"`
	BaseRef         string `json:"base_ref"`
	BaseSHA         string `json:"base_sha"`
	HeadRef         string `json:"head_ref"`
	Status          string `json:"status"`
}

var ErrInvalidOpenResult = errors.New("invalid or unsupported open_pr publication evidence")

func (result OpenResult) Valid() bool {
	return result.Version == 1 && result.PullRequestID > 0 && result.Number > 0 && result.RepositoryID > 0 &&
		bounded(result.NodeID, 256) && bounded(result.HTMLURL, 2048) && bounded(result.RepositoryOwner, 100) && bounded(result.RepositoryName, 100) &&
		bounded(result.BaseRef, 1024) && bounded(result.HeadRef, 1024) && Revision(result.BaseSHA) && Revision(result.HeadSHA) && (result.Status == "OPEN" || result.Status == "CLOSED" || result.Status == "MERGED")
}

func DecodeOpenResult(raw []byte) (OpenResult, error) {
	var result OpenResult
	if len(raw) > 8192 {
		return result, ErrInvalidOpenResult
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || !result.Valid() {
		return OpenResult{}, ErrInvalidOpenResult
	}
	return result, nil
}

func bounded(value string, limit int) bool {
	return len(value) > 0 && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

// Revision accepts full Git object identities only.
func Revision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
