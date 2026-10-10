package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	CompatibilityResultsSchemaVersion   = "omnigrex.runtime-profile-compatibility-results/v1"
	PiCompatibilityResultsSchemaVersion = "omnigrex.runtime-profile-compatibility-results/v2"
	PiCompatibilityQualificationSuite   = "pi-controlled-state-upgrade"
	PiStateContractVersion              = "pi-session/v3"
	CompatibilityQualificationSuite     = "opencode-controlled-state-upgrade"
	CompatibilityQualificationVersion   = "1"
	StateContractVersion                = "opencode-acp-state/v1"
	StableWorkspacePath                 = "/workspace"
)

// CompatibilityResult is one successful qualification of an exact source-to-target upgrade.
type CompatibilityResult struct {
	Source                 Binding  `json:"source"`
	Target                 Binding  `json:"target"`
	Platform               Platform `json:"platform"`
	StateContractVersion   string   `json:"state_contract_version"`
	WorkspacePath          string   `json:"workspace_path"`
	QualificationSuite     string   `json:"qualification_suite"`
	QualificationVersion   string   `json:"qualification_version"`
	QualifiedAt            string   `json:"qualified_at"`
	Outcome                string   `json:"outcome"`
	BackendFamily          string   `json:"backend_family,omitempty"`
	BackendContractVersion int      `json:"backend_contract_version,omitempty"`
}

// CompatibilityResultsFile is the machine-readable release artifact accepted at startup.
type CompatibilityResultsFile struct {
	SchemaVersion string                `json:"schema_version"`
	Results       []CompatibilityResult `json:"results"`
}

// DecodeCompatibilityResultsFile strictly decodes and validates a qualification artifact.
func DecodeCompatibilityResultsFile(reader io.Reader) (CompatibilityResultsFile, error) {
	if reader == nil {
		return CompatibilityResultsFile{}, errors.New("decode Runtime Profile compatibility results: nil reader")
	}
	var file CompatibilityResultsFile
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return CompatibilityResultsFile{}, fmt.Errorf("decode Runtime Profile compatibility results: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CompatibilityResultsFile{}, errors.New("decode Runtime Profile compatibility results: trailing content")
	}
	if err := file.Validate(); err != nil {
		return CompatibilityResultsFile{}, err
	}
	return file, nil
}

// Validate rejects results that do not describe the supported exact compatibility contract.
func (file CompatibilityResultsFile) Validate() error {
	if file.SchemaVersion != CompatibilityResultsSchemaVersion && file.SchemaVersion != PiCompatibilityResultsSchemaVersion {
		return fmt.Errorf("invalid Runtime Profile compatibility results: unsupported schema version %q", file.SchemaVersion)
	}
	if len(file.Results) == 0 {
		return errors.New("invalid Runtime Profile compatibility results: results must not be empty")
	}
	seen := make(map[string]struct{}, len(file.Results))
	for index, result := range file.Results {
		if (result.Source.Name == "pi-rpc") != (file.SchemaVersion == PiCompatibilityResultsSchemaVersion) {
			return errors.New("compatibility schema does not match runtime")
		}
		if err := result.Validate(); err != nil {
			return fmt.Errorf("invalid Runtime Profile compatibility result %d: %w", index, err)
		}
		key, err := result.QualificationKey()
		if err != nil {
			return fmt.Errorf("invalid Runtime Profile compatibility result %d: %w", index, err)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("invalid Runtime Profile compatibility results: duplicate qualification key at result %d", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// ValidateTarget requires every result to qualify the configured Runtime Profile binding and platform.
func (file CompatibilityResultsFile) ValidateTarget(target Profile) error {
	contract := target.Contract()
	if contract.Name == "" {
		return errors.New("configured Runtime Profile is invalid")
	}
	for index, result := range file.Results {
		if result.Target != target.Binding() || result.Platform != contract.Platform {
			return fmt.Errorf("compatibility result %d does not target the configured Runtime Profile binding and platform", index)
		}
	}
	return nil
}

// ValidateDeployment checks exact configured targets and the selected backend.
// Namespace is deliberately excluded: qualification is portable between compatible hosts.
func (file CompatibilityResultsFile) ValidateDeployment(family string, version int, targets ...Profile) error {
	if err := file.Validate(); err != nil {
		return err
	}
	for _, result := range file.Results {
		matched := false
		for _, target := range targets {
			if result.Target == target.Binding() && result.Platform == target.Contract().Platform {
				matched = true
			}
		}
		if !matched {
			return errors.New("compatibility result does not target a configured exact Runtime Profile")
		}
		if result.Source.Name == "pi-rpc" && (result.BackendFamily != family || result.BackendContractVersion != version) {
			return errors.New("compatibility result backend contract mismatch")
		}
	}
	return nil
}

// Validate checks all immutable qualification axes and permits only successful results.
func (result CompatibilityResult) Validate() error {
	if err := result.Source.Validate(); err != nil {
		return fmt.Errorf("source binding: %w", err)
	}
	if err := result.Target.Validate(); err != nil {
		return fmt.Errorf("target binding: %w", err)
	}
	if result.Source == result.Target {
		return errors.New("source and target bindings must differ")
	}
	if result.Platform.OS != "linux" || result.Platform.Arch != "amd64" && result.Platform.Arch != "arm64" {
		return fmt.Errorf("unsupported platform %q/%q", result.Platform.OS, result.Platform.Arch)
	}
	state, suite := StateContractVersion, CompatibilityQualificationSuite
	if result.Source.Name == "pi-rpc" || result.Target.Name == "pi-rpc" {
		if result.Source.Name != "pi-rpc" || result.Target.Name != "pi-rpc" || result.Source.Version != "v1" || result.Target.Version != "v1" {
			return errors.New("Pi qualification requires Pi-to-Pi v1 bindings")
		}
		if (result.BackendFamily != "docker" && result.BackendFamily != "microsandbox") || result.BackendContractVersion != 1 {
			return errors.New("unsupported Pi qualification backend contract")
		}
		state, suite = PiStateContractVersion, PiCompatibilityQualificationSuite
	} else if result.BackendFamily != "" || result.BackendContractVersion != 0 {
		return errors.New("legacy qualification cannot carry Pi backend axes")
	}
	if result.StateContractVersion != state {
		return fmt.Errorf("unsupported state contract version %q", result.StateContractVersion)
	}
	if result.WorkspacePath != StableWorkspacePath {
		return fmt.Errorf("unsupported workspace path %q", result.WorkspacePath)
	}
	if result.QualificationSuite != suite || result.QualificationVersion != CompatibilityQualificationVersion {
		return fmt.Errorf("unsupported qualification suite %q version %q", result.QualificationSuite, result.QualificationVersion)
	}
	qualifiedAt, err := time.Parse(time.RFC3339, result.QualifiedAt)
	if err != nil || !strings.HasSuffix(result.QualifiedAt, "Z") || qualifiedAt.Format(time.RFC3339) != result.QualifiedAt {
		return errors.New("qualified_at must be a canonical UTC RFC3339 timestamp with second precision")
	}
	if result.Outcome != "success" {
		return errors.New("outcome must be success")
	}
	return nil
}

// QualifiedTime returns the validated qualification time.
func (result CompatibilityResult) QualifiedTime() time.Time {
	qualifiedAt, _ := time.Parse(time.RFC3339, result.QualifiedAt)
	return qualifiedAt
}

// ContentSHA256 identifies the canonical complete result, including qualification time.
func (result CompatibilityResult) ContentSHA256() ([sha256.Size]byte, error) {
	if err := result.Validate(); err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode Runtime Profile compatibility result: %w", err)
	}
	return sha256.Sum256(canonical), nil
}

// QualificationKey returns a stable text identity for conflict detection within one artifact.
func (result CompatibilityResult) QualificationKey() (string, error) {
	if err := result.Validate(); err != nil {
		return "", err
	}
	value := struct {
		Source                 Binding  `json:"source"`
		Target                 Binding  `json:"target"`
		Platform               Platform `json:"platform"`
		StateContractVersion   string   `json:"state_contract_version"`
		WorkspacePath          string   `json:"workspace_path"`
		QualificationSuite     string   `json:"qualification_suite"`
		QualificationVersion   string   `json:"qualification_version"`
		BackendFamily          string   `json:"backend_family,omitempty"`
		BackendContractVersion int      `json:"backend_contract_version,omitempty"`
	}{
		Source: result.Source, Target: result.Target, Platform: result.Platform,
		StateContractVersion: result.StateContractVersion, WorkspacePath: result.WorkspacePath,
		QualificationSuite: result.QualificationSuite, QualificationVersion: result.QualificationVersion,
		BackendFamily: result.BackendFamily, BackendContractVersion: result.BackendContractVersion,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
