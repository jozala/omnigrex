package agentprofile_test

import (
	"bytes"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
	"testing"
)

func TestProfileStepBudgetSnapshots(t *testing.T) {
	base := []byte("---\nname: developer\nrole: DEVELOPER\nruntime: pi-rpc/v2\nmodel: openai/test\npermissions:\n  read: allow\n---\nInstructions\n")
	for _, field := range []string{"", "max_steps: unlimited\n", "max_steps: 1\n", "max_steps: 2147483647\n"} {
		content := bytes.Replace(base, []byte("name:"), []byte(field+"name:"), 1)
		p, err := agentprofile.Parse(".omnigrex/team/developer.md", content, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatal(err)
		}
		got, err := agentprofile.DecodeSnapshot(p.CanonicalJSON(), role.BuiltinPolicyCatalog())
		if err != nil || !bytes.Equal(got.CanonicalJSON(), p.CanonicalJSON()) {
			t.Fatalf("snapshot %q: %v", field, err)
		}
		if (got.MaxSteps() == nil) != (field == "") {
			t.Fatalf("omission lost %q", field)
		}
	}
	for _, v := range []string{"0", "-1", "1.5", "null", "", "unknown", "2147483648", "'100'"} {
		if _, err := agentprofile.Parse(".omnigrex/team/developer.md", bytes.Replace(base, []byte("name:"), []byte("max_steps: "+v+"\nname:"), 1), role.BuiltinPolicyCatalog()); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
}
