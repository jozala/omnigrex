// Package piresources identifies the owned resources required by Pi environments.
package piresources

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

//go:embed runner.mjs mcp-extension.mjs failure-extension.mjs subscription-auth.mjs session-state.mjs session-validation.mjs history-rpc.mjs control-events.mjs package.json package-lock.json package-integrity.mjs qualify-packages.mjs
var resources embed.FS

// ResourceContract is the approved inventory captured when a Pi Runtime
// Profile is created. Historical profiles keep it across deployment upgrades.
type ResourceContract struct {
	Version     int            `json:"version"`
	NodeVersion string         `json:"node_version"`
	MiseSHA256  string         `json:"mise_sha256"`
	Files       []ResourceFile `json:"files"`
}

type ResourceFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

func CurrentContract(architecture string) json.RawMessage {
	contract := ResourceContract{Version: 1, NodeVersion: "v24.14.1"}
	switch architecture {
	case "amd64":
		contract.MiseSHA256 = "2af841603ad2981529f230add15c601c64e3a81a79f4f41e9860eb38b7672771"
	case "arm64":
		contract.MiseSHA256 = "a2f1321f1189f2e1e62b1bd0d20fc59ba3c6a0b38b2c64fcfd29296ca44d2905"
	}
	files, _ := resources.ReadDir(".")
	for _, file := range files {
		data, _ := resources.ReadFile(file.Name())
		digest := sha256.Sum256(data)
		contract.Files = append(contract.Files, ResourceFile{Name: file.Name(), SHA256: hex.EncodeToString(digest[:])})
	}
	content, _ := json.Marshal(contract)
	return content
}

var resourceName = regexp.MustCompile(`^[a-z][a-z0-9-]*\.(mjs|json)$`)
var nodeVersion = regexp.MustCompile(`^v[1-9][0-9]*\.[0-9]+\.[0-9]+$`)

// NormalizeContract accepts only bounded, declarative owned-file inventories.
// It does not accept shell commands, paths, links, or image-provided claims.
func NormalizeContract(content json.RawMessage) (json.RawMessage, error) {
	if len(content) == 0 || len(content) > 64<<10 {
		return nil, errors.New("invalid Pi resource contract")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var contract ResourceContract
	if decoder.Decode(&contract) != nil || decoder.Decode(new(any)) != io.EOF || contract.Version != 1 || !nodeVersion.MatchString(contract.NodeVersion) || !resourceHash(contract.MiseSHA256) || len(contract.Files) > 128 {
		return nil, errors.New("invalid Pi resource contract")
	}
	seen := map[string]bool{}
	for _, file := range contract.Files {
		if !resourceName.MatchString(file.Name) || !resourceHash(file.SHA256) || seen[file.Name] {
			return nil, errors.New("invalid Pi resource file")
		}
		seen[file.Name] = true
	}
	for _, name := range []string{"runner.mjs", "mcp-extension.mjs", "failure-extension.mjs", "session-state.mjs", "session-validation.mjs", "history-rpc.mjs", "package.json", "package-lock.json", "package-integrity.mjs", "qualify-packages.mjs"} {
		if !seen[name] {
			return nil, errors.New("incomplete Pi resource contract")
		}
	}
	slices.SortFunc(contract.Files, func(a, b ResourceFile) int { return strings.Compare(a.Name, b.Name) })
	return json.Marshal(contract)
}

func resourceHash(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func QualificationScript(architecture string, content json.RawMessage) (string, error) {
	if len(content) == 0 {
		content = CurrentContract(architecture)
	} // Preparatory pre-Step14 contracts.
	canonical, err := NormalizeContract(content)
	if err != nil {
		return "", err
	}
	var contract ResourceContract
	if err := json.Unmarshal(canonical, &contract); err != nil {
		return "", err
	}
	var script strings.Builder
	script.WriteString(`
for directory in /opt /opt/omnigrex /opt/omnigrex/pi; do
  test -d "$directory" && test ! -L "$directory"
done
`)
	fmt.Fprintf(&script, "test \"$(/usr/local/bin/node --version)\" = '%s'\n", contract.NodeVersion)
	fmt.Fprintf(&script, "printf '%%s  %%s\\n' '%s' /usr/local/bin/mise | sha256sum -c -\n", contract.MiseSHA256)
	for _, file := range contract.Files {
		fmt.Fprintf(&script, "test -f '/opt/omnigrex/pi/%s' && test ! -L '/opt/omnigrex/pi/%s'\n", file.Name, file.Name)
		fmt.Fprintf(&script, "printf '%%s  %%s\\n' '%s' '/opt/omnigrex/pi/%s' | sha256sum -c -\n", file.SHA256, file.Name)
	}
	script.WriteString("/usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /opt/omnigrex/pi/qualify-packages.mjs\n")
	return script.String(), nil
}
