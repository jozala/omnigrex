package agentevent

import "time"

// ToolObservation is one observed terminal invocation, without pairing or
// deduplication state. Error is Pi's final flag, not a specific failure cause.
type ToolObservation struct {
	Layer    string
	Name     string
	Error    bool
	Duration *time.Duration
}

// TemporaryFilesystem is a live filesystem sample, not directory allocation.
// Nil fields are unavailable. No paths, filenames or raw errors are retained.
type TemporaryFilesystem struct {
	Resource   string   `json:"resource"`
	Status     string   `json:"status"`
	Capacity   *float64 `json:"capacity,omitempty"`
	Used       *float64 `json:"used,omitempty"`
	Free       *float64 `json:"free,omitempty"`
	Available  *float64 `json:"available,omitempty"`
	Inodes     *float64 `json:"inodes,omitempty"`
	FreeInodes *float64 `json:"freeInodes,omitempty"`
}
