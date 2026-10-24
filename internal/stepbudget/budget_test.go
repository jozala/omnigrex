package stepbudget

import (
	"encoding/json"
	"testing"
)

func TestLimits(t *testing.T) {
	for _, value := range []string{"unlimited", "1", "100", "2147483647"} {
		l, err := Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(l)
		var got Limit
		if json.Unmarshal(raw, &got) != nil || got != l {
			t.Fatalf("roundtrip %s", value)
		}
	}
	for _, value := range []string{"", "0", "-1", "1.5", "1e2", "2147483648", "9007199254740992", "Unlimited", " 1", "null", "+1"} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, raw := range []string{`null`, `"100"`, `1.0`, `true`, `{}`} {
		var l Limit
		if json.Unmarshal([]byte(raw), &l) == nil {
			t.Fatalf("accepted JSON %s", raw)
		}
	}
	finite, _ := Parse("5")
	unlimited := Limit{}
	if Resolve(nil, finite) != finite || Resolve(&unlimited, finite) != unlimited {
		t.Fatal("precedence")
	}
}
func TestObservationValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"limit":1,"used":2,"responses":2,"summary":true,"stopped":true,"complete":true}`, `{"limit":"unlimited","used":1,"responses":1,"summary":true,"stopped":true,"complete":true}`, `{"limit":1,"used":null,"responses":0,"summary":false,"stopped":false,"complete":false}`} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := Decode([]byte(`{"limit":1,"used":1,"responses":1,"summary":true,"stopped":true,"complete":true}`)); err != nil {
		t.Fatal(err)
	}
}
