// Package repoenvironment owns trusted repository image selection.
package repoenvironment

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"gopkg.in/yaml.v3"
)

const Path = ".omnigrex/environment.yaml"

var ErrInvalid = errors.New("invalid repository environment")

type Definition struct {
	Version      int                     `yaml:"version"`
	Image        string                  `yaml:"image"`
	Platform     string                  `yaml:"platform"`
	Requirements []execution.Requirement `yaml:"requirements"`
}

func Parse(content []byte) (Definition, error) {
	if len(content) == 0 || len(content) > 64<<10 {
		return Definition{}, ErrInvalid
	}
	var definition Definition
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&definition); err != nil {
		return Definition{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Definition{}, ErrInvalid
	}
	if definition.Version != 1 || !profile.IsExactRegistryImage(definition.Image) ||
		(definition.Platform != "linux/amd64" && definition.Platform != "linux/arm64") {
		return Definition{}, ErrInvalid
	}
	seen := map[execution.Requirement]bool{}
	for _, requirement := range definition.Requirements {
		switch requirement {
		case execution.ProcessIsolation, execution.HardwareIsolation, execution.NestedContainers, execution.ToolInstallation:
		default:
			return Definition{}, fmt.Errorf("%w: unknown requirement %q", ErrInvalid, requirement)
		}
		if seen[requirement] {
			return Definition{}, ErrInvalid
		}
		seen[requirement] = true
	}
	slices.Sort(definition.Requirements)
	return definition, nil
}
