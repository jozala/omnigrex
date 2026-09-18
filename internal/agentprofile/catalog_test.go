package agentprofile_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

func TestCatalogAllowsMultipleProfilesForOneRole(t *testing.T) {
	catalog, err := agentprofile.NewCatalog(role.BuiltinCatalog(), []agentprofile.Identity{
		{Name: "primary-developer", Role: role.Developer, Path: ".omnigrex/team/primary-developer.md"},
		{Name: "release-developer", Role: role.Developer, Path: ".omnigrex/team/release-developer.md"},
		{Name: "reviewer", Role: role.Reviewer, Path: ".omnigrex/team/reviewer.md"},
	})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	identities := catalog.Identities()
	if len(identities) != 3 || identities[1].Name != "release-developer" || identities[1].Role != role.Developer {
		t.Fatalf("Identities() = %#v", identities)
	}
	identities[0].Path = "changed"
	identity, _ := catalog.Identity("primary-developer")
	if identity.Path != ".omnigrex/team/primary-developer.md" {
		t.Fatal("Catalog identity was mutated through returned slice")
	}
}

func TestCatalogRejectsInvalidAndDuplicateIdentities(t *testing.T) {
	for _, identities := range [][]agentprofile.Identity{
		nil,
		{{Name: "Developer", Role: role.Developer, Path: ".omnigrex/team/developer.md"}},
		{{Name: "developer", Role: role.ID("UNKNOWN"), Path: ".omnigrex/team/developer.md"}},
		{{Name: "developer", Role: role.Developer, Path: "../developer.md"}},
		{{Name: "developer", Role: role.Developer, Path: ".omnigrex/team/developer.md"}, {Name: "developer", Role: role.Developer, Path: ".omnigrex/team/other.md"}},
	} {
		if _, err := agentprofile.NewCatalog(role.BuiltinCatalog(), identities); !errors.Is(err, agentprofile.ErrInvalidCatalog) {
			t.Errorf("NewCatalog(%#v) error = %v", identities, err)
		}
	}
}

func TestSelectionRequiresExactReferencedRoleCoverage(t *testing.T) {
	catalog, err := agentprofile.NewCatalog(role.BuiltinCatalog(), []agentprofile.Identity{
		{Name: "developer", Role: role.Developer, Path: ".omnigrex/team/developer.md"},
		{Name: "alternate-developer", Role: role.Developer, Path: ".omnigrex/team/alternate-developer.md"},
		{Name: "reviewer", Role: role.Reviewer, Path: ".omnigrex/team/reviewer.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := agentprofile.NewSelection(catalog, []role.ID{role.Developer, role.Reviewer}, map[role.ID]agentprofile.Name{
		role.Developer: "alternate-developer",
		role.Reviewer:  "reviewer",
	})
	if err != nil {
		t.Fatalf("NewSelection() error = %v", err)
	}
	if name, ok := selection.Profile(role.Developer); !ok || name != "alternate-developer" {
		t.Fatalf("Profile(Developer) = (%q, %t)", name, ok)
	}
	roles := selection.Roles()
	if !reflect.DeepEqual(roles, []role.ID{role.Developer, role.Reviewer}) {
		t.Fatalf("Roles() = %#v", roles)
	}
	roles[0] = role.Reviewer
	if current := selection.Roles(); current[0] != role.Developer {
		t.Fatal("Selection Roles were mutated through returned slice")
	}

	invalid := []map[role.ID]agentprofile.Name{
		{role.Developer: "developer"},
		{role.Developer: "reviewer", role.Reviewer: "developer"},
		{role.Developer: "missing", role.Reviewer: "reviewer"},
		{role.Developer: "developer", role.Reviewer: "reviewer", role.ID("EXTRA"): "developer"},
	}
	for _, selected := range invalid {
		if _, err := agentprofile.NewSelection(catalog, []role.ID{role.Developer, role.Reviewer}, selected); !errors.Is(err, agentprofile.ErrInvalidSelection) {
			t.Errorf("NewSelection(%#v) error = %v", selected, err)
		}
	}
}
