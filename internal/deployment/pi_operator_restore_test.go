package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiOperatorRestoreRequiresConfirmedNativeAbsence(t *testing.T) {
	for _, mode := range []string{"nonempty", "unavailable", "empty"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			guide, err := os.ReadFile(filepath.Join(deployTestRepositoryRoot(t), "docs/operator-guide.md"))
			if err != nil {
				t.Fatal(err)
			}
			_, section, found := strings.Cut(string(guide), "## Restore\n")
			if !found {
				t.Fatal("missing authoritative restore procedure")
			}
			_, block, found := strings.Cut(section, "```sh\n")
			if !found {
				t.Fatal("missing restore shell example")
			}
			block, _, found = strings.Cut(block, "\n```")
			if !found {
				t.Fatal("unterminated restore example")
			}
			block = strings.ReplaceAll(block, "'/absolute/path/to/backups/20260907T120000Z'", "'"+root+"'")
			stub := `#!/bin/sh
printf '%s\n' "$*" >> "${0%/*}/calls"
case "$*" in
  *'orchestrator list --quiet')
    case "$MODE" in
      nonempty) printf 'unresolved-native-environment\n';;
      unavailable) exit 42;;
    esac;;
esac
`
			stub = strings.ReplaceAll(stub, `"$MODE"`, `"`+mode+`"`)
			for name, content := range map[string]string{"docker": stub, "shasum": "#!/bin/sh\nexit 0\n", "omnigrex.dump": "fixture", "microsandbox-journal.tar.gz": "fixture"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.CommandContext(t.Context(), "sh", "-c", block)
			command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "BACKEND=microsandbox")
			output, err := command.CombinedOutput()
			calls, readErr := os.ReadFile(filepath.Join(root, "calls"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(calls), "orchestrator list --quiet") {
				t.Fatal("restore did not inspect native inventory before replacing protected state")
			}
			if mode == "empty" {
				if err != nil || !strings.Contains(string(calls), "rm -rf /target/omnigrex-operations") {
					t.Fatalf("confirmed native absence blocked restore: %v %s", err, output)
				}
			} else if err == nil || strings.Contains(string(calls), "volume create") || strings.Contains(string(calls), "dst=/target") {
				t.Fatalf("unresolved native inventory allowed protected-state replacement: %v %s", err, output)
			}
		})
	}
}
