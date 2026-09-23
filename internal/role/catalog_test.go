package role_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/role"
)

func TestCatalogOwnsCanonicalRoleIdentities(t *testing.T) {
	catalog, err := role.NewCatalog([]role.Metadata{
		{ID: role.Developer, DisplayName: "Developer", Description: "Implements requested changes."},
		{ID: role.Reviewer, DisplayName: "Reviewer", Description: "Evaluates the Change Proposal."},
	})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}

	if !catalog.Contains(role.Developer) || !catalog.Contains(role.Reviewer) {
		t.Fatalf("catalog does not contain built-in Roles")
	}
	if catalog.Contains(role.ID("SECURITY_REVIEWER")) {
		t.Fatal("catalog unexpectedly contains an unregistered Role")
	}
	if got := catalog.IDs(); !reflect.DeepEqual(got, []role.ID{role.Developer, role.Reviewer}) {
		t.Fatalf("IDs() = %v, want Developer and Reviewer in declaration order", got)
	}
	metadata, ok := catalog.Lookup(role.Reviewer)
	if !ok || metadata.DisplayName != "Reviewer" || metadata.Description != "Evaluates the Change Proposal." {
		t.Fatalf("Lookup(Reviewer) = (%#v, %t)", metadata, ok)
	}
}

func TestCatalogRejectsInvalidRoleDefinitions(t *testing.T) {
	tests := []struct {
		name     string
		metadata []role.Metadata
	}{
		{name: "empty", metadata: nil},
		{name: "invalid ID", metadata: []role.Metadata{{ID: "security-reviewer", DisplayName: "Security Reviewer", Description: "Reviews security."}}},
		{name: "blank display name", metadata: []role.Metadata{{ID: "SECURITY_REVIEWER", Description: "Reviews security."}}},
		{name: "blank description", metadata: []role.Metadata{{ID: "SECURITY_REVIEWER", DisplayName: "Security Reviewer"}}},
		{name: "duplicate", metadata: []role.Metadata{
			{ID: "SECURITY_REVIEWER", DisplayName: "Security Reviewer", Description: "Reviews security."},
			{ID: "SECURITY_REVIEWER", DisplayName: "Another", Description: "Duplicate."},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := role.NewCatalog(test.metadata); !errors.Is(err, role.ErrInvalidCatalog) {
				t.Fatalf("NewCatalog() error = %v, want ErrInvalidCatalog", err)
			}
		})
	}
}

func TestCatalogResultsCannotMutateCatalog(t *testing.T) {
	definitions := []role.Metadata{{ID: role.Developer, DisplayName: "Developer", Description: "Implements changes."}}
	catalog, err := role.NewCatalog(definitions)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}

	definitions[0].DisplayName = "Mutated"
	ids := catalog.IDs()
	ids[0] = role.Reviewer

	metadata, ok := catalog.Lookup(role.Developer)
	if !ok || metadata.DisplayName != "Developer" || !catalog.Contains(role.Developer) {
		t.Fatalf("catalog was mutated: (%#v, %t)", metadata, ok)
	}
}
