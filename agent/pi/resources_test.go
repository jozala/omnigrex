package piresources

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceQualificationUsesRetainedInventory(t *testing.T) {
	var old ResourceContract
	if err := json.Unmarshal(CurrentContract("amd64"), &old); err != nil {
		t.Fatal(err)
	}
	old.NodeVersion = "v24.13.0"
	old.MiseSHA256 = strings.Repeat("b", 64)
	old.Files[0].SHA256 = strings.Repeat("a", 64)
	content, _ := json.Marshal(old)
	script, err := QualificationScript("amd64", content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "v24.13.0") || !strings.Contains(script, old.MiseSHA256) || !strings.Contains(script, old.Files[0].SHA256) {
		t.Fatal("current deployment replaced retained resource inventory")
	}
	current, err := QualificationScript("amd64", CurrentContract("amd64"))
	if err != nil {
		t.Fatal(err)
	}
	if script == current {
		t.Fatal("source and current resource checks became indistinguishable")
	}
}

func TestResourceContractsRejectExecutableOrIncompleteInventory(t *testing.T) {
	for _, mutate := range []func(*ResourceContract){
		func(c *ResourceContract) { c.Version = 2 }, func(c *ResourceContract) { c.NodeVersion = "v24.14.1'; touch /tmp/poison; #" }, func(c *ResourceContract) { c.MiseSHA256 = "unverified" }, func(c *ResourceContract) { c.Files[0].Name = "../runner.mjs" }, func(c *ResourceContract) { c.Files[0].SHA256 = "unverified" }, func(c *ResourceContract) { c.Files = c.Files[:1] }, func(c *ResourceContract) { c.Files = append(c.Files, c.Files[0]) },
	} {
		var contract ResourceContract
		_ = json.Unmarshal(CurrentContract("arm64"), &contract)
		mutate(&contract)
		content, _ := json.Marshal(contract)
		if _, err := QualificationScript("arm64", content); err == nil {
			t.Fatal("unsafe resource contract accepted")
		}
	}
	if _, err := NormalizeContract(json.RawMessage(`{"version":1,"command":"sh"}`)); err == nil {
		t.Fatal("unknown resource command accepted")
	}
}
