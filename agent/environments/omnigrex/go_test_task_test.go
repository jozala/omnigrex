package development_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGoTestTaskPreservesArgumentsCachesAndExitStatus(t *testing.T) {
	script := "/opt/qualification/go-test.sh"
	if os.Getenv("OMNIGREX_DEVELOPMENT_CACHE_TEST") != "1" {
		var err error
		script, err = filepath.Abs("../../../scripts/go-test.sh")
		if err != nil {
			t.Fatal(err)
		}
	}
	tmp, goTmp := "/allocated/turn-tmp", "/allocated/go-tmp"
	if info, err := os.Stat("/opt/omnigrex/go-proxy"); err == nil && info.IsDir() {
		tmp, goTmp = "/var/tmp", "/var/tmp"
	}
	for _, exit := range []int{0, 17} {
		t.Run(fmt.Sprintf("exit-%d", exit), func(t *testing.T) {
			bin := t.TempDir()
			fakeGo := "#!/bin/sh\nprintf '%s\\0' \"$TMPDIR\" \"$GOTMPDIR\" \"$GOPATH\" \"$GOCACHE\" \"$@\"\nexit \"$FAKE_GO_EXIT\"\n"
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "sh", script, "-race", "-run", "case with spaces", "./...")
			command.Dir = bin
			command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=/allocated/turn-tmp", "GOTMPDIR=/allocated/go-tmp",
				"GOPATH=/allocated/go-modules", "GOCACHE=/allocated/go-build", fmt.Sprintf("FAKE_GO_EXIT=%d", exit)}
			output, err := command.CombinedOutput()
			if exit == 0 && err != nil {
				t.Fatalf("run Go task: %v\n%s", err, output)
			}
			if exit != 0 {
				failure, ok := err.(*exec.ExitError)
				if !ok || failure.ExitCode() != exit {
					t.Fatalf("task exit = %v; want %d", err, exit)
				}
			}
			got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
			want := []string{tmp, goTmp, "/allocated/go-modules", "/allocated/go-build", "test", "-count=1", "-race", "-run", "case with spaces", "./..."}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go task environment/arguments = %q; want %q", got, want)
			}
		})
	}
}
