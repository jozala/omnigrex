package github

import (
	"reflect"
	"testing"
)

func TestManagedLabelsAreStable(t *testing.T) {
	want := [...]Label{
		{Name: "omnigrex:run", Color: "1f6feb", Description: "Start or resume Omnigrex work"},
		{Name: "omnigrex:developing", Color: "d4a72c", Description: "Omnigrex Developer is working"},
		{Name: "omnigrex:reviewing", Color: "8250df", Description: "Omnigrex Reviewer is reviewing"},
		{Name: "omnigrex:pr-ready", Color: "2da44e", Description: "Change Proposal is ready for human review"},
		{Name: "omnigrex:needs-human", Color: "cf222e", Description: "Omnigrex needs human attention"},
	}
	if !reflect.DeepEqual(managedLabels, want) {
		t.Errorf("managed labels = %#v, want %#v", managedLabels, want)
	}
}
