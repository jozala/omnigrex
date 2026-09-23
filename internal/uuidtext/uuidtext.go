// Package uuidtext defines the UUID text policies used at external and destructive boundaries.
package uuidtext

import "github.com/google/uuid"

// Valid reports whether value is a standard hyphenated UUID, preserving its original spelling.
func Valid(value string) bool {
	if len(value) != 36 {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

// Canonicalize parses standard hyphenated UUID text and returns its lowercase canonical spelling.
func Canonicalize(value string) (string, bool) {
	if len(value) != 36 {
		return "", false
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// ValidCanonicalNonNil reports whether value is an exact lowercase, non-nil UUID.
func ValidCanonicalNonNil(value string) bool {
	parsed, ok := Canonicalize(value)
	return ok && parsed == value && parsed != uuid.Nil.String()
}

// NewRandom returns a random RFC 4122 version 4 UUID without panicking on entropy failure.
func NewRandom() (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return value.String(), nil
}
