package github

import (
	"fmt"
	"time"
)

type ConfigurationError struct {
	Cause error
}

func (err *ConfigurationError) Error() string {
	return fmt.Sprintf("invalid GitHub configuration: %v", err.Cause)
}

func (err *ConfigurationError) Unwrap() error {
	return err.Cause
}

func (err *ConfigurationError) Permanent() bool {
	return true
}

type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
	RequestID  string
}

func (err *APIError) Error() string {
	message := err.Message
	if message == "" {
		message = "request failed"
	}
	if err.RequestID != "" {
		return fmt.Sprintf("GitHub API %s %s returned %d: %s (request %s)", err.Method, err.Path, err.StatusCode, message, err.RequestID)
	}
	return fmt.Sprintf("GitHub API %s %s returned %d: %s", err.Method, err.Path, err.StatusCode, message)
}

type NotInstalledError struct {
	Owner          string
	Repository     string
	InstallationID int64
	Cause          error
}

func (err *NotInstalledError) Error() string {
	if err.Owner != "" || err.Repository != "" {
		return fmt.Sprintf("GitHub App is not installed for repository %s/%s", err.Owner, err.Repository)
	}
	return fmt.Sprintf("GitHub App installation %d is not available", err.InstallationID)
}

func (err *NotInstalledError) Unwrap() error {
	return err.Cause
}

func (err *NotInstalledError) Permanent() bool {
	return true
}

type PermissionError struct {
	*APIError
}

func (err *PermissionError) Permanent() bool {
	return true
}

type RateLimitError struct {
	*APIError
	RetryAfter time.Duration
	ResetAt    time.Time
}

func (err *RateLimitError) Transient() bool {
	return true
}

type TransientError struct {
	Cause error
}

func (err *TransientError) Error() string {
	return fmt.Sprintf("transient GitHub API failure: %v", err.Cause)
}

func (err *TransientError) Unwrap() error {
	return err.Cause
}

func (err *TransientError) Transient() bool {
	return true
}
