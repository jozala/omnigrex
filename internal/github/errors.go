package github

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// SafeErrorMetadata contains the credential-free error properties needed for retry decisions.
type SafeErrorMetadata struct {
	Permanent      bool
	Transient      bool
	APIClientError bool
	APIRetryable   bool
	RetryAfter     time.Duration
	ResetAt        time.Time
}

// SafeErrorMetadataProvider exposes retry properties without exposing an underlying error.
type SafeErrorMetadataProvider interface {
	SafeErrorMetadata() SafeErrorMetadata
}

// ExtractSafeErrorMetadata copies only allowlisted retry properties from an error chain.
func ExtractSafeErrorMetadata(err error) SafeErrorMetadata {
	var metadata SafeErrorMetadata
	var provider SafeErrorMetadataProvider
	if errors.As(err, &provider) {
		metadata = provider.SafeErrorMetadata()
	}
	var permanent interface{ Permanent() bool }
	if errors.As(err, &permanent) && permanent.Permanent() {
		metadata.Permanent = true
	}
	var transient interface{ Transient() bool }
	if errors.As(err, &transient) && transient.Transient() {
		metadata.Transient = true
	}
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.StatusCode >= http.StatusBadRequest && apiError.StatusCode < http.StatusInternalServerError {
		metadata.APIClientError = true
		metadata.APIRetryable = apiError.StatusCode == http.StatusRequestTimeout || apiError.StatusCode == http.StatusTooManyRequests
	}
	var rateLimit *RateLimitError
	if errors.As(err, &rateLimit) {
		if rateLimit.APIError != nil && rateLimit.StatusCode >= http.StatusBadRequest && rateLimit.StatusCode < http.StatusInternalServerError {
			metadata.APIClientError = true
			metadata.APIRetryable = rateLimit.StatusCode == http.StatusRequestTimeout || rateLimit.StatusCode == http.StatusTooManyRequests
		}
		metadata.RetryAfter = rateLimit.RetryAfter
		metadata.ResetAt = rateLimit.ResetAt.UTC()
	}
	if !metadata.ResetAt.IsZero() {
		metadata.ResetAt = metadata.ResetAt.UTC()
	}
	return metadata
}

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
