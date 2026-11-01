package main

import "testing"

func TestVersionSyntax(t *testing.T) {
	for _, value := range []string{"1.0.0", "0.0.0", "v0.1.0", "0.01.0", "0.1.0-beta.1", "0.1.0-alpha.0", "0.1.0-alpha.01", "0.1.0-alpha.1\n", "0.999999999999999999999999999999999999999999.0-alpha.1"} {
		t.Run(value, func(t *testing.T) {
			if _, e := parseVersion(value, true); e == nil {
				t.Fatal("invalid alpha accepted")
			}
		})
	}
	for _, value := range []string{"0.1.0", "0.2.5-alpha.1", "0.3", "0.0.9"} {
		if _, e := parseVersion(value, false); (e == nil) != (value == "0.1.0") {
			t.Fatalf("stable %s: %v", value, e)
		}
	}
}
func TestVersionProgression(t *testing.T) {
	stable := []Release{{TagName: "v0.1.0"}}
	for _, tc := range []struct {
		value                    string
		alpha, breaking, success bool
		tags                     []Tag
	}{
		{"0.1.1-alpha.1", true, false, true, nil}, {"0.2.0-alpha.3", true, true, true, nil}, {"0.1.0", false, false, false, nil},
		{"0.1.1-alpha.1", true, true, false, nil}, {"0.1.1-alpha.1", true, false, false, []Tag{{Name: "v0.1.1-alpha.1"}}},
		{"0.2.1-alpha.1", true, true, false, nil}, {"0.2.0", false, true, true, nil},
	} {
		t.Run(tc.value, func(t *testing.T) {
			e := progression(tc.value, tc.alpha, tc.breaking, stable, tc.tags)
			if (e == nil) != tc.success {
				t.Fatalf("progression error: %v", e)
			}
		})
	}
	assertError(t, progression("0.2.5", false, false, []Release{{TagName: "v0.3.0"}}, nil), "move forward")
	releases := append(stable, Release{TagName: "v0.2.0-alpha.3", Prerelease: true})
	assertError(t, progression("0.2.0-alpha.2", true, true, releases, nil), "sequence")
	if e := progression("0.2.0-alpha.4", true, true, releases, nil); e != nil {
		t.Fatal(e)
	}
	assertError(t, progression("0.2.0-alpha.3", true, false, releases, nil), "already exists")
	assertError(t, progression("0.2.0-alpha.1", true, false, append(stable, Release{TagName: "v0.2.0-alpha.1", Draft: true}), nil), "already exists")
}
