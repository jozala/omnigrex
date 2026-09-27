// Package turnconfig validates repository requests for disk-backed Runtime Process paths.
package turnconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const Path = ".omnigrex/turn-configuration.yaml"
const maxSize = 64 << 10

var ErrInvalid = errors.New("invalid turn configuration")

type Lifecycle string

const (
	Turn       Lifecycle = "turn"
	Assignment Lifecycle = "assignment"
)

// Configuration contains only path requests, never repository-supplied path values.
type Configuration struct {
	Directories map[string]Lifecycle `json:"directories"`
	Environment map[string]string    `json:"environment"`
}

var directoryName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*$`)

func ValidDirectory(name string) bool { return directoryName.MatchString(name) }

func SafeVariable(name string) bool {
	if !environmentName.MatchString(name) || len(name) > 128 {
		return false
	}
	if name == "TMPDIR" {
		return true
	}
	for _, prefix := range []string{"OPENCODE_", "OMNIGREX_", "MISE_", "XDG_", "LD_", "DYLD_", "GIT_"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	switch name {
	case "PATH", "HOME", "ENV", "BASH_ENV", "SHELLOPTS", "PYTHONPATH", "PYTHONHOME", "NODE_OPTIONS", "JAVA_TOOL_OPTIONS", "SSL_CERT_FILE":
		return false
	}
	return true
}

// Allowlist parses the operator's comma-separated set; empty means no variables are allowed.
func Allowlist(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) > 128 {
		return nil, ErrInvalid
	}
	seen := make(map[string]bool, len(parts))
	for _, name := range parts {
		if name != strings.TrimSpace(name) || !SafeVariable(name) || seen[name] {
			return nil, fmt.Errorf("%w: invalid operator allowlist entry %q", ErrInvalid, name)
		}
		seen[name] = true
	}
	return parts, nil
}

func Parse(content []byte, allowed []string) (Configuration, error) {
	if len(content) == 0 || len(content) > maxSize {
		return Configuration{}, fmt.Errorf("%w: file is empty or exceeds 64 KiB", ErrInvalid)
	}
	var document struct {
		Version int `yaml:"version"`
		Paths   struct {
			Directories map[string]struct {
				Lifecycle Lifecycle `yaml:"lifecycle"`
			} `yaml:"directories"`
			Environment []struct {
				Name      string `yaml:"name"`
				Directory string `yaml:"directory"`
			} `yaml:"environment"`
		} `yaml:"environment-paths"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var remainder any
	if err := decoder.Decode(&remainder); !errors.Is(err, io.EOF) {
		return Configuration{}, fmt.Errorf("%w: multiple YAML documents", ErrInvalid)
	}
	if document.Version != 1 || len(document.Paths.Directories) == 0 || len(document.Paths.Directories) > 64 ||
		len(document.Paths.Environment) == 0 || len(document.Paths.Environment) > 128 {
		return Configuration{}, fmt.Errorf("%w: version, directories or environment entries", ErrInvalid)
	}
	config := Configuration{Directories: make(map[string]Lifecycle), Environment: make(map[string]string)}
	for name, directory := range document.Paths.Directories {
		if !directoryName.MatchString(name) || directory.Lifecycle != Turn && directory.Lifecycle != Assignment {
			return Configuration{}, fmt.Errorf("%w: directory %q", ErrInvalid, name)
		}
		config.Directories[name] = directory.Lifecycle
	}
	permitted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		permitted[name] = true
	}
	for _, entry := range document.Paths.Environment {
		if !SafeVariable(entry.Name) || !permitted[entry.Name] {
			return Configuration{}, fmt.Errorf("%w: variable %q is not allowed", ErrInvalid, entry.Name)
		}
		if _, exists := config.Directories[entry.Directory]; !exists {
			return Configuration{}, fmt.Errorf("%w: variable %q has no declared directory", ErrInvalid, entry.Name)
		}
		if _, duplicate := config.Environment[entry.Name]; duplicate {
			return Configuration{}, fmt.Errorf("%w: duplicate variable %q", ErrInvalid, entry.Name)
		}
		config.Environment[entry.Name] = entry.Directory
	}
	if err := config.Validate(allowed); err != nil {
		return Configuration{}, err
	}
	return config, nil
}

// Validate checks a persisted configuration again against the active operator policy.
func (config Configuration) Validate(allowed []string) error {
	if len(config.Directories) == 0 && len(config.Environment) == 0 {
		return nil
	}
	if len(config.Directories) == 0 || len(config.Directories) > 64 || len(config.Environment) == 0 || len(config.Environment) > 128 {
		return ErrInvalid
	}
	for name, lifecycle := range config.Directories {
		if !ValidDirectory(name) || lifecycle != Turn && lifecycle != Assignment {
			return ErrInvalid
		}
	}
	permitted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		permitted[name] = true
	}
	for name, directory := range config.Environment {
		if !SafeVariable(name) || !permitted[name] {
			return fmt.Errorf("%w: variable %q is not allowed", ErrInvalid, name)
		}
		if _, exists := config.Directories[directory]; !exists {
			return ErrInvalid
		}
	}
	return nil
}
