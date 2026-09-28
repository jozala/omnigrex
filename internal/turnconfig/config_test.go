package turnconfig_test

import (
	"testing"

	"github.com/jozala/omnigrex/internal/turnconfig"
)

func TestRepositoryCanRequestSharedScratchAndPersistentCache(t *testing.T) {
	configuration, err := turnconfig.Parse([]byte(`version: 1
environment-paths:
  directories:
    build-tmp:
      lifecycle: turn
    go-cache:
      lifecycle: assignment
  environment:
    - name: TMPDIR
      directory: build-tmp
    - name: GOTMPDIR
      directory: build-tmp
    - name: GOCACHE
      directory: go-cache
`), []string{"TMPDIR", "GOTMPDIR", "GOCACHE"})
	if err != nil {
		t.Fatal(err)
	}
	if got := configuration.Environment["GOTMPDIR"]; got != "build-tmp" || configuration.Directories["go-cache"] != turnconfig.Assignment {
		t.Fatalf("validated configuration = %#v", configuration)
	}
}

func TestRepositoryCannotConfigureUnapprovedOrUnsupportedPaths(t *testing.T) {
	for _, test := range []struct{ name, text string }{
		{"unapproved", "version: 1\nenvironment-paths:\n  directories:\n    tmp: {lifecycle: turn}\n  environment:\n    - {name: GOPATH, directory: tmp}\n"},
		{"reserved", "version: 1\nenvironment-paths:\n  directories:\n    tmp: {lifecycle: turn}\n  environment:\n    - {name: OPENCODE_CONFIG_CONTENT, directory: tmp}\n"},
		{"future field", "version: 1\nenvironment-paths:\n  directories:\n    tmp: {lifecycle: turn, size: 128MiB}\n  environment:\n    - {name: TMPDIR, directory: tmp}\n"},
		{"duplicate binding", "version: 1\nenvironment-paths:\n  directories:\n    tmp: {lifecycle: turn}\n  environment:\n    - {name: TMPDIR, directory: tmp}\n    - {name: TMPDIR, directory: tmp}\n"},
		{"unknown directory", "version: 1\nenvironment-paths:\n  directories:\n    tmp: {lifecycle: turn}\n  environment:\n    - {name: TMPDIR, directory: other}\n"},
		{"unsafe directory", "version: 1\nenvironment-paths:\n  directories:\n    ../tmp: {lifecycle: turn}\n  environment:\n    - {name: TMPDIR, directory: ../tmp}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := turnconfig.Parse([]byte(test.text), []string{"TMPDIR", "OPENCODE_CONFIG_CONTENT"}); err == nil {
				t.Fatal("unsafe repository configuration was accepted")
			}
		})
	}
}

func TestOperatorAllowlistRejectsRuntimeControlVariables(t *testing.T) {
	for _, text := range []string{"OPENCODE_CONFIG_CONTENT", "HOME", "PATH", "TMPDIR,TMPDIR", "GOCACHE, GOPATH"} {
		if _, err := turnconfig.Allowlist(text); err == nil {
			t.Fatalf("allowlist %q accepted", text)
		}
	}
	allowed, err := turnconfig.Allowlist("TMPDIR,GOTMPDIR,GOCACHE,GOPATH")
	if err != nil || len(allowed) != 4 {
		t.Fatalf("valid allowlist = (%#v, %v)", allowed, err)
	}
}
