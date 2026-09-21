package agentprofile

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jozala/omnigrex/internal/role"
)

var (
	ErrInvalidCatalog   = errors.New("invalid Agent Profile catalog")
	ErrInvalidSelection = errors.New("invalid Agent Profile selection")
)

// Identity identifies an Agent Profile and its repository location.
type Identity struct {
	Name Name
	Role role.ID
	Path string
}

// Catalog is an immutable set of Agent Profile identities.
type Catalog struct {
	ordered []Name
	entries map[Name]Identity
}

func NewCatalog(identities []Identity) (Catalog, error) {
	if len(identities) == 0 {
		return Catalog{}, fmt.Errorf("%w: no identities", ErrInvalidCatalog)
	}
	catalog := Catalog{
		ordered: make([]Name, 0, len(identities)), entries: make(map[Name]Identity, len(identities)),
	}
	for _, identity := range identities {
		if !validName(identity.Name) || !role.ValidID(identity.Role) || !validPath(identity.Path) {
			return Catalog{}, fmt.Errorf("%w: invalid identity %q", ErrInvalidCatalog, identity.Name)
		}
		if _, duplicate := catalog.entries[identity.Name]; duplicate {
			return Catalog{}, fmt.Errorf("%w: duplicate name %q", ErrInvalidCatalog, identity.Name)
		}
		catalog.ordered = append(catalog.ordered, identity.Name)
		catalog.entries[identity.Name] = identity
	}
	return catalog, nil
}

func (catalog Catalog) Identity(name Name) (Identity, bool) {
	identity, ok := catalog.entries[name]
	return identity, ok
}

func (catalog Catalog) Identities() []Identity {
	identities := make([]Identity, 0, len(catalog.ordered))
	for _, name := range catalog.ordered {
		identities = append(identities, catalog.entries[name])
	}
	return identities
}

// Selection chooses exactly one catalog Profile for each referenced Role.
type Selection struct {
	roles  []role.ID
	byRole map[role.ID]Name
	byName map[Name]role.ID
}

// Selector chooses one Agent Profile for every Role from a discovered catalog.
type Selector interface {
	Select(Catalog, role.PolicyCatalog) (Selection, error)
}

// SingletonSelector accepts a catalog only when exactly one Profile belongs to every Role.
type SingletonSelector struct{}

func (SingletonSelector) Select(catalog Catalog, policies role.PolicyCatalog) (Selection, error) {
	return NewSingletonSelection(catalog, policies)
}

func NewSelection(catalog Catalog, roles []role.ID, selected map[role.ID]Name) (Selection, error) {
	if len(roles) == 0 || len(catalog.entries) == 0 {
		return Selection{}, fmt.Errorf("%w: empty catalog or Role set", ErrInvalidSelection)
	}
	selection := Selection{roles: make([]role.ID, 0, len(roles)), byRole: make(map[role.ID]Name, len(roles)), byName: make(map[Name]role.ID, len(roles))}
	seenRoles := make(map[role.ID]struct{}, len(roles))
	for _, roleID := range roles {
		if _, duplicate := seenRoles[roleID]; duplicate {
			return Selection{}, fmt.Errorf("%w: duplicate Role %q", ErrInvalidSelection, roleID)
		}
		seenRoles[roleID] = struct{}{}
		name, ok := selected[roleID]
		identity, exists := catalog.Identity(name)
		if !ok || !exists || identity.Role != roleID {
			return Selection{}, fmt.Errorf("%w: Role %q", ErrInvalidSelection, roleID)
		}
		if selectedRole, duplicate := selection.byName[name]; duplicate && selectedRole != roleID {
			return Selection{}, fmt.Errorf("%w: Profile %q selected by multiple Roles", ErrInvalidSelection, name)
		}
		selection.roles = append(selection.roles, roleID)
		selection.byRole[roleID] = name
		selection.byName[name] = roleID
	}
	if len(selected) != len(selection.byRole) {
		return Selection{}, fmt.Errorf("%w: selection contains unreferenced Roles", ErrInvalidSelection)
	}
	return selection, nil
}

// NewSingletonSelection selects a Role only when exactly one catalog Profile belongs to it.
func NewSingletonSelection(catalog Catalog, policies role.PolicyCatalog) (Selection, error) {
	selected := make(map[role.ID]Name, len(policies.Roles()))
	for _, identity := range catalog.Identities() {
		if !policies.Contains(identity.Role) {
			return Selection{}, fmt.Errorf("%w: Profile %q has unreferenced Role %q", ErrInvalidSelection, identity.Name, identity.Role)
		}
		if existing, duplicate := selected[identity.Role]; duplicate {
			return Selection{}, fmt.Errorf("%w: Role %q has multiple Profiles %q and %q", ErrInvalidSelection, identity.Role, existing, identity.Name)
		}
		selected[identity.Role] = identity.Name
	}
	return NewSelection(catalog, policies.Roles(), selected)
}

func (selection Selection) Profile(roleID role.ID) (Name, bool) {
	name, ok := selection.byRole[roleID]
	return name, ok
}

func (selection Selection) Roles() []role.ID {
	return append([]role.ID(nil), selection.roles...)
}

func validName(name Name) bool {
	value := string(name)
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value[1:] {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func validPath(value string) bool {
	const prefix = ".omnigrex/team/"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, ".md") || strings.TrimSpace(value) != value {
		return false
	}
	name := strings.TrimPrefix(value, prefix)
	if len(name) <= len(".md") || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ValidPath reports whether value is a direct Markdown file under the Agent Profile directory.
func ValidPath(value string) bool { return validPath(value) }
