package deployment_test

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiUpgradeDoesNotPublishFailedCopies(t *testing.T) {
	for _, outputKind := range []string{"compatibility", "state"} {
		for _, contents := range []string{"empty", "partial"} {
			t.Run(outputKind+"/"+contents, func(t *testing.T) {
				root := t.TempDir()
				payload := filepath.Join(root, "payload.tar")
				var data bytes.Buffer
				writer := tar.NewWriter(&data)
				if err := writer.WriteHeader(&tar.Header{Name: "output", Mode: 0600, Size: 7, Typeflag: tar.TypeReg}); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write([]byte("partial")); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(payload, data.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				stub := `#!/bin/sh
case "$1 $2" in
  'image inspect') case "$*" in *Architecture*) printf 'arm64\n';; *) printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';; esac;;
  'run --rm') case "$*" in *stat*) printf '0\n';; esac;;
  'inspect '*) printf '0\n';;
  'cp '*)
    if [ "$COPY_CONTENTS" = partial ]; then
      if [ "$3" = - ]; then /bin/cat "$COPY_PAYLOAD"; else printf 'partial' > "$3"; fi
    fi
    exit 42;;
esac
exit 0
`
				if err := os.WriteFile(filepath.Join(root, "docker"), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(root, "result")
				command := exec.CommandContext(t.Context(), "sh", "../../scripts/test-pi-upgrade.sh")
				command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root, "OMNIGREX_PI_BACKEND=docker", "OMNIGREX_PI_UPGRADE_SOURCE_IMAGE=registry.example/pi@sha256:"+strings.Repeat("a", 64), "OMNIGREX_PI_UPGRADE_TARGET_IMAGE=registry.example/pi@sha256:"+strings.Repeat("b", 64), "OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE=", "OMNIGREX_PI_UPGRADE_EXPORT_ARCHIVE=", "OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE=", "OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE=", "COPY_CONTENTS="+contents, "COPY_PAYLOAD="+payload)
				variable := "OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE"
				if outputKind == "state" {
					variable = "OMNIGREX_PI_UPGRADE_EXPORT_ARCHIVE"
				}
				command.Env = append(command.Env, variable+"="+output)
				if logs, err := command.CombinedOutput(); err == nil {
					t.Fatalf("failed copy reported success: %s", logs)
				}
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("failed copy published output: %v", err)
				}
			})
		}
	}
}
