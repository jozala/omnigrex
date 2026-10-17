package developmentenvironment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestComposeQualificationRequiresSuccessfulEmptyInventories(t *testing.T) {
	script, err := filepath.Abs("../qualify-development-compose.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode string
		exit int
	}{
		{"empty", 0},
		{"fail-containers", 42}, {"fail-networks", 42}, {"fail-volumes", 42},
		{"survive-containers", 1}, {"survive-networks", 1}, {"survive-volumes", 1},
	} {
		t.Run(test.mode, func(t *testing.T) {
			root := t.TempDir()
			stub := `#!/bin/sh
set -eu
case "$1" in
  compose) exit 0 ;;
  ps) kind=containers ;;
  network) kind=networks ;;
  volume) kind=volumes ;;
  *) exit 99 ;;
esac
case "$REVIEW_MODE" in
  "fail-$kind") exit 42 ;;
  "survive-$kind") printf 'surviving-resource\n' ;;
esac
`
			if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "sh", script)
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "REVIEW_MODE="+test.mode)
			output, err := command.CombinedOutput()
			if test.exit == 0 {
				if err != nil {
					t.Fatalf("empty inventories failed qualification: %v\n%s", err, output)
				}
				return
			}
			failure, ok := err.(*exec.ExitError)
			if !ok || failure.ExitCode() != test.exit {
				t.Fatalf("qualification exit = %v; want %d\n%s", err, test.exit, output)
			}
		})
	}
}
