package collaboration

import "fmt"

// References are permanent Work Item names, not shortened internal identities.
// Allocation is persisted with reservations under the existing Workflow lock.
// Failed reservations remain in history and consume their reference.
type Reference struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

func (s *State) reference(kind, id string) string {
	count := 0
	for _, ref := range s.References {
		if ref.Kind == kind {
			count++
			if ref.ID == id {
				return ref.Name
			}
		}
	}
	name := fmt.Sprintf("%s%d", kind, count+1)
	s.References = append(s.References, Reference{Name: name, ID: id, Kind: kind})
	return name
}

func (s *State) target(kind, name string) string {
	for _, ref := range s.References {
		if ref.Kind == kind && ref.Name == name {
			return ref.ID
		}
	}
	return ""
}
