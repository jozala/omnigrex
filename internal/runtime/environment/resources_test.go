package environment

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQualificationComposesWithStorageSetupScript(t *testing.T) {
	script, err := QualificationScript("amd64", "pi-rpc")
	if err != nil {
		t.Fatal(err)
	}
	// Docker's storage initializer intentionally ends without a newline.
	// Resource qualification must remain a separate shell statement.
	command := exec.Command("/bin/sh", "-n")
	command.Stdin = strings.NewReader("for path in fixture; do :; done" + script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("invalid combined initializer: %v\n%s", err, output)
	}
}
