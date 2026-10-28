// Package reviewpolicy defines repository-owned scheduling, not review authority.
package reviewpolicy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const Path = ".omnigrex/review-policy.yaml"
const MaximumTimeout = 2 * time.Hour
const MaximumGrace = 5 * time.Minute

var ErrInvalid = errors.New("invalid review policy")

type Selector struct {
	Context string `yaml:"context" json:"context"`
	Source  string `yaml:"source" json:"source"`
	AppID   int64  `yaml:"app-id,omitempty" json:"app_id,omitempty"`
}
type Policy struct {
	Start             string        `json:"start"`
	Selection         string        `json:"selection"`
	Timeout           time.Duration `json:"timeout"`
	RegistrationGrace time.Duration `json:"registration_grace"`
	Checks            []Selector    `json:"checks,omitempty"`
}

func Default() Policy {
	return Policy{Start: "after_ci", Selection: "github_required", Timeout: 30 * time.Minute, RegistrationGrace: time.Minute}
}

func Parse(content []byte) (Policy, error) {
	if len(content) == 0 || len(content) > 64<<10 {
		return Policy{}, ErrInvalid
	}
	var syntax yaml.Node
	if err := yaml.Unmarshal(content, &syntax); err != nil {
		return Policy{}, ErrInvalid
	}
	var valid func(*yaml.Node) bool
	valid = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode || n.Tag == "!!null" || n.Kind == yaml.ScalarNode && n.Value == "" {
			return false
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "app-id" {
					id, err := strconv.ParseInt(n.Content[i+1].Value, 10, 64)
					if err != nil || id <= 0 || n.Content[i+1].Tag != "!!int" {
						return false
					}
				}
			}
		}
		for _, child := range n.Content {
			if !valid(child) {
				return false
			}
		}
		return true
	}
	if !valid(&syntax) {
		return Policy{}, ErrInvalid
	}
	var doc struct {
		Version int `yaml:"version"`
		Review  struct {
			Start string `yaml:"start"`
			CI    struct {
				Selection string      `yaml:"selection"`
				Timeout   string      `yaml:"timeout"`
				Grace     string      `yaml:"registration-grace"`
				Checks    *[]Selector `yaml:"checks"`
			} `yaml:"ci"`
		} `yaml:"review"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Policy{}, ErrInvalid
	}
	if doc.Version != 1 {
		return Policy{}, ErrInvalid
	}
	p := Default()
	if doc.Review.Start != "" {
		p.Start = doc.Review.Start
	}
	if doc.Review.CI.Selection != "" {
		p.Selection = doc.Review.CI.Selection
	}
	var err error
	if doc.Review.CI.Timeout != "" {
		p.Timeout, err = time.ParseDuration(doc.Review.CI.Timeout)
		if err != nil {
			return Policy{}, ErrInvalid
		}
	}
	if doc.Review.CI.Grace != "" {
		p.RegistrationGrace, err = time.ParseDuration(doc.Review.CI.Grace)
		if err != nil {
			return Policy{}, ErrInvalid
		}
	}
	if doc.Review.CI.Checks != nil {
		if p.Selection != "explicit" {
			return Policy{}, ErrInvalid
		}
		p.Checks = *doc.Review.CI.Checks
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}
func (p Policy) Validate() error {
	if p.Start != "after_ci" && p.Start != "immediate" || p.Selection != "github_required" && p.Selection != "explicit" || p.Timeout < time.Second || p.Timeout > MaximumTimeout || p.RegistrationGrace < 0 || p.RegistrationGrace > MaximumGrace || p.RegistrationGrace > p.Timeout || p.Timeout%time.Second != 0 || p.RegistrationGrace%time.Second != 0 {
		return ErrInvalid
	}
	if p.Selection == "explicit" && (len(p.Checks) == 0 || len(p.Checks) > 100) || p.Selection != "explicit" && len(p.Checks) != 0 {
		return ErrInvalid
	}
	seen := map[Selector]bool{}
	for _, c := range p.Checks {
		if strings.TrimSpace(c.Context) != c.Context || c.Context == "" || len(c.Context) > 256 || strings.ContainsAny(c.Context, "\r\n\x00") || c.Source != "check" && c.Source != "status" || c.AppID < 0 || c.Source == "status" && c.AppID != 0 || seen[c] {
			return ErrInvalid
		}
		seen[c] = true
	}
	return nil
}
