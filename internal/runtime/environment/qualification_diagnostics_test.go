package environment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func TestQualificationOutputBoundary(t *testing.T) {
	resources := piresources.CandidateContract("amd64")
	for _, test := range []struct {
		name, text      string
		observed, known bool
	}{
		{"complete", "private-token=fake\nPi resource check failed: owned checksum runner.mjs\n", true, true},
		{"split", "Pi resource check failed: Node version\n", true, true},
		{"no exit evidence", "Pi resource check failed: Node version\n", false, false},
		{"truncated", "Pi resource check failed: Node version", true, false},
		{"unexpected", "Pi resource check failed: secret-name.mjs\n", true, false},
		{"suffix", "Pi resource check failed: Node version token=fake\n", true, false},
		{"binary", "Pi resource check failed: Node version\x00\n", true, false},
		{"oversized line", strings.Repeat("x", 257) + "\nPi resource check failed: Node version\n", true, false},
		{"oversized stream", strings.Repeat("x\n", 8193) + "Pi resource check failed: Node version\n", true, false},
		{"unexpected after valid", "Pi resource check failed: Node version\nPi resource check failed: token=fake\n", true, false},
		{"malformed after valid", "Pi resource check failed: Node version\nPi resource check failed:Node version\n", true, false},
		{"conflicting", "Pi resource check failed: Node version\nPi resource check failed: mise checksum\n", true, false},
		{"trailing partial", "Pi resource check failed: Node version\npartial", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := NewQualificationOutput("amd64", "pi-rpc", resources)
			// Exercise every event/chunk boundary, not just line-at-once parsing.
			for _, b := range []byte(test.text) {
				_, _ = output.Write([]byte{b})
			}
			if test.observed {
				output.ObserveNonzeroExit()
			}
			got := output.Check()
			if strings.Contains(got, "guest-reported") != test.known || strings.Contains(got, "fake") || strings.Contains(got, "secret-name") {
				t.Fatal(got)
			}
			if len(output.line) > 256 || len(got) > 256 {
				t.Fatal("unbounded diagnostic")
			}
		})
	}
}

func TestQualificationFailureUsesOriginalBindingAndPreservesSemantics(t *testing.T) {
	var inventory piresources.ResourceContract
	_ = json.Unmarshal(piresources.CandidateContract("arm64"), &inventory)
	inventory.NodeVersion = "v24.13.0"
	historical, _ := json.Marshal(inventory)
	canonical, _ := piresources.NormalizeContract(historical)
	image := "registry.example/historical@sha256:" + strings.Repeat("a", 64)
	for _, cause := range []error{execution.ErrUnknownEffect, context.Canceled, context.DeadlineExceeded, execution.ErrNotFound, errors.New("native release failed")} {
		got := QualificationFailure(image, "linux", "arm64", historical, nil, cause)
		if !errors.Is(got, cause) {
			t.Fatal(got)
		}
		for _, want := range []string{image, "linux/arm64", fmt.Sprintf("sha256:%x", sha256.Sum256(canonical)), "unknown stage", "do not rebind"} {
			if !strings.Contains(got.Error(), want) {
				t.Fatalf("missing %q: %v", want, got)
			}
		}
		if strings.Contains(got.Error(), "selected production") {
			t.Fatal("retained contract replaced with current selection")
		}
	}
	if got := QualificationFailure(image, "linux", "arm64", nil, nil, errors.New("missing inventory")); !strings.Contains(got.Error(), "missing/invalid bound inventory") {
		t.Fatal(got)
	}
}

func TestQualificationScriptReportsActualFailedCheck(t *testing.T) {
	for _, test := range []struct {
		name, check string
		mutate      func(string)
	}{
		{"compatible", "", func(string) {}},
		{"missing executable", "executable /usr/local/bin/rg", func(root string) { _ = os.Remove(filepath.Join(root, "bin/rg")) }},
		{"bootstrap", "bootstrap identity", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "bin/omnigrex-environment"), []byte("#!/bin/sh\necho incompatible\n"), 0700)
		}},
		{"node", "Node version", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "bin/node"), []byte("#!/bin/sh\necho v1.0.0\n"), 0700)
		}},
		{"mise", "mise checksum", func(root string) { _ = os.WriteFile(filepath.Join(root, "bin/mise"), []byte("corrupt"), 0700) }},
		{"owned checksum", "owned checksum runner.mjs", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "owned/pi/runner.mjs"), []byte("corrupt"), 0600)
		}},
		{"owned file", "owned checksum runner.mjs", func(root string) { _ = os.Remove(filepath.Join(root, "owned/pi/runner.mjs")) }},
		{"symlink", "owned file runner.mjs", func(root string) {
			_ = os.Remove(filepath.Join(root, "owned/pi/runner.mjs"))
			_ = os.Symlink(filepath.Join(root, "owned/pi/package.json"), filepath.Join(root, "owned/pi/runner.mjs"))
		}},
		{"package integrity", "locked package integrity", func(root string) {
			_ = os.WriteFile(filepath.Join(root, "bin/node"), []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo v24.14.1; else echo fake-secret >&2; exit 1; fi\n"), 0700)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"bin", "owned/pi"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var inventory piresources.ResourceContract
			_ = json.Unmarshal(piresources.CandidateContract("amd64"), &inventory)
			for _, executable := range inventory.Executables {
				if !strings.HasPrefix(executable, "/usr/local/bin/") {
					continue
				}
				body := "#!/bin/sh\nexit 0\n"
				switch filepath.Base(executable) {
				case "node":
					body = "#!/bin/sh\nif [ \"$1\" = --version ]; then echo v24.14.1; fi\n"
				case "omnigrex-environment":
					body = "#!/bin/sh\necho omnigrex-environment/v1\n"
				}
				if err := os.WriteFile(filepath.Join(root, "bin", filepath.Base(executable)), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
				if filepath.Base(executable) == "mise" {
					inventory.MiseSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
				}
			}
			for i := range inventory.Files {
				inventory.Files[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("fixture")))
				if err := os.WriteFile(filepath.Join(root, "owned/pi", inventory.Files[i].Name), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resources, _ := json.Marshal(inventory)
			script, err := QualificationScript("amd64", "pi-rpc", resources)
			if err != nil {
				t.Fatal(err)
			}
			// Relocate fixture execution only; preserve host-generated diagnostic labels.
			replace := strings.NewReplacer("/usr/local/bin", filepath.Join(root, "bin"), "/opt/omnigrex/pi", filepath.Join(root, "owned/pi"), "/opt/omnigrex", filepath.Join(root, "owned"), "/opt", root)
			lines := strings.Split(script, "\n")
			for i, line := range lines {
				if !strings.HasPrefix(line, "omnigrex_check=") {
					lines[i] = replace.Replace(line)
				}
			}
			test.mutate(root)
			output := NewQualificationOutput("amd64", "pi-rpc", resources)
			command := exec.Command("/bin/sh", "-ec", strings.Join(lines, "\n"))
			command.Stderr = output
			err = command.Run()
			if test.check == "" {
				if err != nil {
					t.Fatal(err, output.Check())
				}
				return
			}
			if err == nil {
				t.Fatal("corrupt resource qualified")
			}
			output.ObserveNonzeroExit()
			if got := output.Check(); got != test.check+" (guest-reported failed check)" {
				t.Fatalf("got %q want %q", got, test.check)
			}
		})
	}
}
