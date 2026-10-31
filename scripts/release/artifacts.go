package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
)

const repository = "jozala/omnigrex"

var products = []string{"orchestrator", "orchestrator-microsandbox", "pi"}
var deploymentFiles = []string{"compose.pi.yaml", "compose.pi-docker.yaml", "compose.pi-microsandbox.yaml", ".env.pi.example"}
var requiredJobs = []string{"docker-check", "development-qualification", "microsandbox", "check", "publish", "deploy-smoke", "deploy-smoke-microsandbox", "development-smoke-microsandbox"}
var candidateAssets = []string{"payload.tar.gz", "candidate.json", "SHA256SUMS"}
var releaseAssets = []string{"payload.tar.gz", "candidate.json", "SHA256SUMS", "release.json"}

// Build identity is channel-independent and embedded unchanged in the payload.
type Build struct {
	SchemaVersion int               `json:"schema_version"`
	Repository    string            `json:"repository"`
	Commit        string            `json:"commit"`
	RunID         int64             `json:"run_id"`
	Attempt       int               `json:"attempt"`
	Platform      string            `json:"platform"`
	Images        map[string]string `json:"images"`
}
type Manifest struct {
	Build
	Files         map[string]string `json:"files"`
	PayloadSHA256 string            `json:"payload_sha256"`
	Qualification map[string]int64  `json:"qualification,omitempty"`
}
type Designation struct {
	Version       string `json:"version"`
	Channel       string `json:"channel"`
	Breaking      string `json:"breaking"`
	Upgrades      string `json:"upgrades"`
	Commit        string `json:"commit"`
	PayloadSHA256 string `json:"payload_sha256"`
	PromotedFrom  string `json:"promoted_from,omitempty"`
}

func encode(v any) ([]byte, error) {
	b, e := json.MarshalIndent(v, "", "  ")
	return append(b, '\n'), e
}
func writeJSON(dir, name string, v any) error {
	b, e := encode(v)
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0644)
}
func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func readAsset(dir, name string) ([]byte, error) {
	f, e := os.Open(filepath.Join(dir, name))
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if len(b) > 8<<20 {
		return nil, errors.New("release asset exceeds size limit")
	}
	return b, e
}
func validateBuild(b Build) error {
	if b.SchemaVersion != 1 || b.Repository != repository || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(b.Commit) || b.RunID <= 0 || b.Attempt <= 0 || b.Platform != "linux/amd64" {
		return errors.New("invalid candidate schema, repository, commit, run/attempt or platform")
	}
	if len(b.Images) != len(products) {
		return errors.New("product images must exclude development and include both controllers and Pi")
	}
	for _, n := range products {
		if !regexp.MustCompile(`^ghcr.io/jozala/omnigrex/` + n + `@sha256:[0-9a-f]{64}$`).MatchString(b.Images[n]) {
			return fmt.Errorf("invalid exact image digest: %s", n)
		}
	}
	return nil
}
func checksumBytes(dir string) ([]byte, error) {
	var out bytes.Buffer
	for _, n := range candidateAssets[:2] {
		b, e := readAsset(dir, n)
		if e != nil {
			return nil, e
		}
		fmt.Fprintf(&out, "%s  %s\n", sum(b), n)
	}
	return out.Bytes(), nil
}
func writeChecksums(dir string) error {
	b, e := checksumBytes(dir)
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), b, 0644)
}
func pack(dir, sourceDir string, b Build) error {
	if e := validateBuild(b); e != nil {
		return e
	}
	if e := os.Mkdir(dir, 0755); e != nil {
		return e
	}
	files := map[string][]byte{}
	for _, n := range deploymentFiles {
		data, e := os.ReadFile(filepath.Join(sourceDir, "deploy", n))
		if e != nil {
			return e
		}
		files["deploy/"+n] = data
	}
	data, e := encode(b)
	if e != nil {
		return e
	}
	files["build.json"] = data
	var payload bytes.Buffer
	gz := gzip.NewWriter(&payload)
	tw := tar.NewWriter(gz)
	// Fixed order keeps newly generated archives reproducible; promotion never repacks.
	names := []string{"build.json"}
	for _, n := range deploymentFiles {
		names = append(names, "deploy/"+n)
	}
	m := Manifest{Build: b, Files: map[string]string{}}
	for _, n := range names {
		data := files[n]
		m.Files[n] = sum(data)
		if e := tw.WriteHeader(&tar.Header{Name: n, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}); e != nil {
			return e
		}
		if _, e := tw.Write(data); e != nil {
			return e
		}
	}
	if e := tw.Close(); e != nil {
		return e
	}
	if e := gz.Close(); e != nil {
		return e
	}
	m.PayloadSHA256 = sum(payload.Bytes())
	if e := os.WriteFile(filepath.Join(dir, "payload.tar.gz"), payload.Bytes(), 0644); e != nil {
		return e
	}
	if e := writeJSON(dir, "candidate.json", m); e != nil {
		return e
	}
	if e := writeChecksums(dir); e != nil {
		return e
	}
	_, _, e = verify(dir)
	return e
}

// validatePayload returns only allowlisted regular files. It never extracts paths supplied by an archive.
func validatePayload(payload []byte, m Manifest) (map[string][]byte, error) {
	expected := map[string]bool{"build.json": true}
	for _, n := range deploymentFiles {
		expected["deploy/"+n] = true
	}
	if len(m.Files) != len(expected) {
		return nil, errors.New("unexpected deployment file manifest")
	}
	for n := range m.Files {
		if !expected[n] {
			return nil, errors.New("unexpected deployment file manifest")
		}
	}
	gz, e := gzip.NewReader(bytes.NewReader(payload))
	if e != nil {
		return nil, e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if !expected[h.Name] || files[h.Name] != nil || h.Typeflag != tar.TypeReg || h.Mode != 0644 || h.Size < 0 || h.Size > 1<<20 {
			return nil, errors.New("unsafe archive path, duplicate file or entry type/size/mode")
		}
		b, e := io.ReadAll(tr)
		if e != nil {
			return nil, e
		}
		if sum(b) != m.Files[h.Name] {
			return nil, fmt.Errorf("archive file checksum mismatch: %s", h.Name)
		}
		files[h.Name] = b
	}
	// Consume gzip through its checksum/footer. tar EOF alone can precede gzip
	// EOF; permit only bounded zero padding after the tar end marker.
	padding, e := io.ReadAll(io.LimitReader(gz, (1<<20)+1))
	if e != nil {
		return nil, fmt.Errorf("invalid archive compression: %w", e)
	}
	if len(padding) > 1<<20 || len(bytes.Trim(padding, "\x00")) != 0 {
		return nil, errors.New("unexpected trailing archive data")
	}
	if len(files) != len(expected) {
		return nil, errors.New("archive is missing deployment files")
	}
	var build Build
	if e := strictJSON(files["build.json"], &build); e != nil {
		return nil, e
	}
	if !reflect.DeepEqual(build, m.Build) {
		return nil, errors.New("build identity/digest mismatch")
	}
	return files, nil
}
func verify(dir string) (Manifest, map[string][]byte, error) {
	var m Manifest
	b, e := readAsset(dir, "candidate.json")
	if e != nil {
		return m, nil, e
	}
	if e := strictJSON(b, &m); e != nil {
		return m, nil, e
	}
	if e := validateBuild(m.Build); e != nil {
		return m, nil, e
	}
	payload, e := readAsset(dir, "payload.tar.gz")
	if e != nil {
		return m, nil, e
	}
	if sum(payload) != m.PayloadSHA256 {
		return m, nil, errors.New("payload checksum mismatch")
	}
	files, e := validatePayload(payload, m)
	if e != nil {
		return m, nil, e
	}
	actual, e := readAsset(dir, "SHA256SUMS")
	if e != nil {
		return m, nil, e
	}
	expected, e := checksumBytes(dir)
	if e != nil {
		return m, nil, e
	}
	if !bytes.Equal(actual, expected) {
		return m, nil, errors.New("asset checksums mismatch")
	}
	return m, files, nil
}
