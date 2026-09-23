package role

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const maxIDLength = 64

var ErrInvalidCatalog = errors.New("invalid Role Catalog")

// ID is the canonical durable identity of a Role.
type ID string

const (
	Developer ID = "DEVELOPER"
	Reviewer  ID = "REVIEWER"
)

// Metadata describes a Role without owning subsystem-specific behavior.
type Metadata struct {
	ID          ID
	DisplayName string
	Description string
}

// Catalog is an immutable ordered collection of known Roles.
type Catalog struct {
	ordered []ID
	byID    map[ID]Metadata
}

// NewCatalog validates and copies Role metadata into an immutable Catalog.
func NewCatalog(metadata []Metadata) (Catalog, error) {
	if len(metadata) == 0 {
		return Catalog{}, fmt.Errorf("%w: no Roles", ErrInvalidCatalog)
	}
	catalog := Catalog{ordered: make([]ID, 0, len(metadata)), byID: make(map[ID]Metadata, len(metadata))}
	for _, definition := range metadata {
		if !ValidID(definition.ID) || strings.TrimSpace(definition.DisplayName) == "" ||
			strings.TrimSpace(definition.DisplayName) != definition.DisplayName || strings.TrimSpace(definition.Description) == "" ||
			strings.TrimSpace(definition.Description) != definition.Description || containsControl(definition.DisplayName) ||
			containsControl(definition.Description) {
			return Catalog{}, fmt.Errorf("%w: invalid metadata for %q", ErrInvalidCatalog, definition.ID)
		}
		if _, duplicate := catalog.byID[definition.ID]; duplicate {
			return Catalog{}, fmt.Errorf("%w: duplicate Role %q", ErrInvalidCatalog, definition.ID)
		}
		catalog.ordered = append(catalog.ordered, definition.ID)
		catalog.byID[definition.ID] = definition
	}
	return catalog, nil
}

// BuiltinCatalog returns the Roles supported by the built-in Workflow Definition.
func BuiltinCatalog() Catalog {
	catalog, err := NewCatalog([]Metadata{
		{ID: Developer, DisplayName: "Developer", Description: "Implements and revises the Change Proposal."},
		{ID: Reviewer, DisplayName: "Reviewer", Description: "Evaluates the current Change Proposal."},
	})
	if err != nil {
		panic(err)
	}
	return catalog
}

// ValidID reports whether value has the canonical durable Role ID syntax.
func ValidID(value ID) bool {
	if len(value) == 0 || len(value) > maxIDLength || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, character := range value[1:] {
		if character < 'A' || character > 'Z' {
			if character < '0' || character > '9' {
				if character != '_' {
					return false
				}
			}
		}
	}
	return true
}

// Contains reports whether the Role is registered in the Catalog.
func (catalog Catalog) Contains(id ID) bool {
	_, ok := catalog.byID[id]
	return ok
}

// Lookup returns descriptive metadata for a registered Role.
func (catalog Catalog) Lookup(id ID) (Metadata, bool) {
	metadata, ok := catalog.byID[id]
	return metadata, ok
}

// IDs returns registered Role IDs in declaration order.
func (catalog Catalog) IDs() []ID {
	return append([]ID(nil), catalog.ordered...)
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
