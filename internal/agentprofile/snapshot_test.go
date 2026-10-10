package agentprofile_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

func TestSnapshotRoundTripPreservesEscapedInstructions(t *testing.T) {
	for _, instructions := range []string{strings.Repeat("<", 50000), strings.Repeat("<>&", 80000)} {
		source := []byte("---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v1\nmodel: controlled/workflow\npermissions:\n  read: allow\n---\n" + instructions)
		p, err := agentprofile.Parse(".omnigrex/team/developer.md", source, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatal(err)
		}
		snapshot := p.CanonicalJSON()
		if len(snapshot) <= agentprofile.MaxContentSize {
			t.Fatal("fixture did not exercise JSON expansion")
		}
		decoded, err := agentprofile.DecodeSnapshot(snapshot, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatalf("valid source (%d bytes), snapshot (%d bytes) rejected: %v", len(source), len(snapshot), err)
		}
		if decoded.Instructions() != instructions || !bytes.Equal(decoded.CanonicalJSON(), snapshot) {
			t.Fatal("snapshot changed validated Profile values")
		}
	}
}

func TestSnapshotRetainsBoundsAndPresenceSensitivePiSchema(t *testing.T) {
	source := []byte("---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v1\nmodel: controlled/workflow\npermissions:\n  read: allow\n---\nInstructions")
	p, err := agentprofile.Parse(".omnigrex/team/developer.md", source, role.BuiltinPolicyCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range []map[string]any{{"instructions": strings.Repeat("x", agentprofile.MaxContentSize+1)}, {"steps": 0}, {"variant": ""}, {"STEPS": 0}, {"unexpected": "value"}} {
		var fields map[string]any
		_ = json.Unmarshal(p.CanonicalJSON(), &fields)
		for key, value := range patch {
			fields[key] = value
		}
		encoded, _ := json.Marshal(fields)
		if _, err := agentprofile.DecodeSnapshot(encoded, role.BuiltinPolicyCatalog()); err == nil {
			t.Fatalf("invalid snapshot accepted: keys %+v", keysForSnapshotTest(patch))
		}
	}
}

func keysForSnapshotTest(fields map[string]any) []string {
	var keys []string
	for key := range fields {
		keys = append(keys, key)
	}
	return keys
}
