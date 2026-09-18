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

// Identity is the code-defined authority and repository location of an Agent Profile.
type Identity struct {
	Name Name
	Role role.ID
	Path string
}

// Catalog is an immutable set of Agent Profile identities.
type Catalog struct {
	ordered  []Name
	entries  map[Name]Identity
	policies map[Name]role.Policy
}

func NewCatalog(roles role.Catalog, identities []Identity) (Catalog, error) {
	if len(roles.IDs()) == 0 || len(identities) == 0 {
		return Catalog{}, fmt.Errorf("%w: no identities", ErrInvalidCatalog)
	}
	catalog := Catalog{
		ordered: make([]Name, 0, len(identities)), entries: make(map[Name]Identity, len(identities)),
		policies: make(map[Name]role.Policy, len(identities)),
	}
	builtinPolicies := role.BuiltinPolicyCatalog()
	for _, identity := range identities {
		if !validName(identity.Name) || !roles.Contains(identity.Role) || !validPath(identity.Path) {
			return Catalog{}, fmt.Errorf("%w: invalid identity %q", ErrInvalidCatalog, identity.Name)
		}
		if _, duplicate := catalog.entries[identity.Name]; duplicate {
			return Catalog{}, fmt.Errorf("%w: duplicate name %q", ErrInvalidCatalog, identity.Name)
		}
		catalog.ordered = append(catalog.ordered, identity.Name)
		catalog.entries[identity.Name] = identity
		policy, ok := builtinPolicies.Lookup(identity.Role)
		if !ok {
			return Catalog{}, fmt.Errorf("%w: no policy for identity %q", ErrInvalidCatalog, identity.Name)
		}
		policy.AgentProfile = role.AgentProfileIdentity{Name: string(identity.Name), Path: identity.Path}
		catalog.policies[identity.Name] = policy
	}
	return catalog, nil
}

// NewCatalogFromPolicies derives Agent Profile identities from the startup-validated Role Policy Catalog.
func NewCatalogFromPolicies(policies role.PolicyCatalog) (Catalog, error) {
	identities := make([]Identity, 0, len(policies.Roles()))
	byName := make(map[Name]Identity, len(policies.Roles()))
	policyByName := make(map[Name]role.Policy, len(policies.Roles()))
	for _, roleID := range policies.Roles() {
		policy, ok := policies.Lookup(roleID)
		if !ok {
			return Catalog{}, fmt.Errorf("%w: missing Role policy %q", ErrInvalidCatalog, roleID)
		}
		identity := Identity{Name: Name(policy.AgentProfile.Name), Role: roleID, Path: policy.AgentProfile.Path}
		if !validName(identity.Name) || !validPath(identity.Path) {
			return Catalog{}, fmt.Errorf("%w: invalid identity %q", ErrInvalidCatalog, identity.Name)
		}
		if _, duplicate := byName[identity.Name]; duplicate {
			return Catalog{}, fmt.Errorf("%w: duplicate name %q", ErrInvalidCatalog, identity.Name)
		}
		identities = append(identities, identity)
		byName[identity.Name] = identity
		policyByName[identity.Name] = policy
	}
	if len(identities) == 0 {
		return Catalog{}, fmt.Errorf("%w: no identities", ErrInvalidCatalog)
	}
	ordered := make([]Name, len(identities))
	for index, identity := range identities {
		ordered[index] = identity.Name
	}
	return Catalog{ordered: ordered, entries: byName, policies: policyByName}, nil
}

func BuiltinCatalog() Catalog {
	catalog, err := NewCatalogFromPolicies(role.BuiltinPolicyCatalog())
	if err != nil {
		panic(err)
	}
	return catalog
}

func (catalog Catalog) policy(name Name) (role.Policy, bool) {
	policy, ok := catalog.policies[name]
	return policy, ok
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

func NewSelection(catalog Catalog, roles []role.ID, selected map[role.ID]Name) (Selection, error) {
	if len(roles) == 0 || len(catalog.entries) == 0 {
		return Selection{}, fmt.Errorf("%w: empty catalog or Role set", ErrInvalidSelection)
	}
	selection := Selection{roles: make([]role.ID, 0, len(roles)), byRole: make(map[role.ID]Name, len(roles)), byName: make(map[Name]role.ID, len(roles))}
	seenRoles := make(map[role.ID]struct{}, len(roles))
	for _, roleID := range roles {
		if _, duplicate := seenRoles[roleID]; duplicate {
			continue
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

func BuiltinSelection() Selection {
	selection, err := NewSelection(BuiltinCatalog(), []role.ID{role.Developer, role.Reviewer}, map[role.ID]Name{
		role.Developer: Developer,
		role.Reviewer:  Reviewer,
	})
	if err != nil {
		panic(err)
	}
	return selection
}

// NewSelectionFromPolicies selects each Role's policy-owned Agent Profile identity.
func NewSelectionFromPolicies(catalog Catalog, policies role.PolicyCatalog) (Selection, error) {
	selected := make(map[role.ID]Name, len(policies.Roles()))
	for _, roleID := range policies.Roles() {
		policy, ok := policies.Lookup(roleID)
		if !ok {
			return Selection{}, fmt.Errorf("%w: missing Role policy %q", ErrInvalidSelection, roleID)
		}
		selected[roleID] = Name(policy.AgentProfile.Name)
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
	return strings.HasPrefix(value, ".omnigrex/team/") && strings.HasSuffix(value, ".md") &&
		!strings.Contains(value, "..") && strings.TrimSpace(value) == value
}
