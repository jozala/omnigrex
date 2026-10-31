package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type Repo struct {
	FullName string `json:"full_name"`
}
type Run struct {
	ID             int64  `json:"id"`
	Attempt        int    `json:"run_attempt"`
	Commit         string `json:"head_sha"`
	Repository     Repo   `json:"repository"`
	HeadRepository Repo   `json:"head_repository"`
	Event          string `json:"event"`
	Branch         string `json:"head_branch"`
	Path           string `json:"path"`
	WorkflowID     int64  `json:"workflow_id"`
	Status         string `json:"status"`
	Conclusion     string `json:"conclusion"`
}
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	RunID      int64  `json:"run_id"`
	Attempt    int    `json:"run_attempt"`
	Commit     string `json:"head_sha"`
}
type Asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type Artifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
}
type Tag struct {
	Name string `json:"name"`
}
type Ref struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}
type Release struct {
	ID         int64   `json:"id,omitempty"`
	TagName    string  `json:"tag_name"`
	Target     string  `json:"target_commitish,omitempty"`
	Name       string  `json:"name,omitempty"`
	Body       string  `json:"body,omitempty"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets,omitempty"`
}

// Missing booleans must not turn unavailable publication/expiry evidence into
// false ("published" / "not expired") through Go's zero values.
func requiredFields(data []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(data, &fields); e != nil {
		return e
	}
	for _, name := range names {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			return fmt.Errorf("GitHub response missing required field: %s", name)
		}
	}
	return nil
}
func (r *Release) UnmarshalJSON(data []byte) error {
	if e := requiredFields(data, "tag_name", "draft", "prerelease"); e != nil {
		return e
	}
	type releaseAlias Release
	return json.Unmarshal(data, (*releaseAlias)(r))
}
func (a *Artifact) UnmarshalJSON(data []byte) error {
	if e := requiredFields(data, "id", "name", "expired"); e != nil {
		return e
	}
	type artifactAlias Artifact
	return json.Unmarshal(data, (*artifactAlias)(a))
}

type publication struct {
	api       githubAPI
	createTag func(context.Context, string, string) error
	upload    func(context.Context, string, []string) error
}
type publishInput struct{ Channel, Version, Notes, Breaking, Upgrades, SourceRun, Alpha string }

func qualification(ctx context.Context, api githubAPI, b Build, names []string) (map[string]int64, error) {
	jobs, e := pages[Job](ctx, api, endpoint(fmt.Sprintf("actions/runs/%d/attempts/%d/jobs", b.RunID, b.Attempt)), "jobs")
	if e != nil {
		return nil, e
	}
	evidence := map[string]int64{}
	for _, name := range names {
		count := 0
		for _, j := range jobs {
			if j.Name == name {
				count++
				if j.ID <= 0 || j.Status != "completed" || j.Conclusion != "success" {
					return nil, fmt.Errorf("required qualification unavailable or unsuccessful: %s", name)
				}
				if j.RunID != b.RunID || j.Attempt != b.Attempt || j.Commit != b.Commit {
					return nil, fmt.Errorf("qualification run/attempt/commit mismatch: %s", name)
				}
				evidence[name] = j.ID
			}
		}
		if count != 1 {
			return nil, fmt.Errorf("required qualification missing or ambiguous: %s", name)
		}
	}
	return evidence, nil
}
func ready(ctx context.Context, api githubAPI, dir string, identity Build) error {
	m, _, e := verify(dir)
	if e != nil {
		return e
	}
	identity.Images = m.Images
	if !reflect.DeepEqual(m.Build, identity) {
		return errors.New("candidate identity mismatch")
	}
	m.Qualification, e = qualification(ctx, api, m.Build, requiredJobs)
	if e != nil {
		return e
	}
	if e := writeJSON(dir, "candidate.json", m); e != nil {
		return e
	}
	return writeChecksums(dir)
}
func validateSource(ctx context.Context, api githubAPI, run Run, m Manifest) error {
	if run.Repository.FullName != repository || run.HeadRepository.FullName != repository {
		return errors.New("source repository mismatch")
	}
	if run.Event != "push" || run.Branch != "main" || run.Path != ".github/workflows/ci.yaml" {
		return errors.New("source must be main push CI")
	}
	workflow, e := getJSON[struct {
		ID int64 `json:"id"`
	}](ctx, api, endpoint("actions/workflows/ci.yaml"))
	if e != nil {
		return e
	}
	if run.WorkflowID != workflow.ID || workflow.ID <= 0 {
		return errors.New("source workflow mismatch")
	}
	if run.Status != "completed" || run.Conclusion != "success" {
		return errors.New("source CI is not successfully complete")
	}
	if run.ID != m.RunID || run.Attempt != m.Attempt || run.Commit != m.Commit {
		return errors.New("source commit/run/attempt mismatch")
	}
	compare, e := getJSON[struct {
		Status string `json:"status"`
	}](ctx, api, endpoint("compare/"+m.Commit+"...main"))
	if e != nil {
		return e
	}
	if compare.Status != "ahead" && compare.Status != "identical" {
		return errors.New("source is not an ancestor of main")
	}
	evidence, e := qualification(ctx, api, m.Build, requiredJobs)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(evidence, m.Qualification) {
		return errors.New("qualification job identity mismatch")
	}
	_, e = qualification(ctx, api, m.Build, []string{"release-candidate"})
	return e
}
func downloadCandidate(ctx context.Context, api githubAPI, run Run, dir string) error {
	artifacts, e := pages[Artifact](ctx, api, endpoint(fmt.Sprintf("actions/runs/%d/artifacts", run.ID)), "artifacts")
	if e != nil {
		return e
	}
	name := fmt.Sprintf("release-candidate-%d-%d", run.ID, run.Attempt)
	var found []Artifact
	for _, a := range artifacts {
		if a.Name == name && !a.Expired {
			found = append(found, a)
		}
	}
	if len(found) != 1 || found[0].ID <= 0 {
		return errors.New("candidate artifact missing, expired, or ambiguous")
	}
	b, e := api.call(ctx, "GET", endpoint(fmt.Sprintf("actions/artifacts/%d/zip", found[0].ID)), nil)
	if e != nil {
		return fmt.Errorf("candidate artifact unavailable: %w", e)
	}
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		return fmt.Errorf("malformed candidate artifact: %w", e)
	}
	if len(z.File) != len(candidateAssets) {
		return errors.New("invalid candidate artifact files")
	}
	seen := map[string]bool{}
	for _, f := range z.File {
		allowed := false
		for _, n := range candidateAssets {
			if f.Name == n {
				allowed = true
			}
		}
		if !allowed || seen[f.Name] || !f.Mode().IsRegular() || f.UncompressedSize64 > 8<<20 {
			return errors.New("invalid candidate artifact paths or size")
		}
		seen[f.Name] = true
		reader, e := f.Open()
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(reader, 8<<20+1))
		closeErr := reader.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if len(data) > 8<<20 {
			return errors.New("candidate artifact file too large")
		}
		if e := os.WriteFile(filepath.Join(dir, f.Name), data, 0644); e != nil {
			return e
		}
	}
	return nil
}
func downloadRelease(ctx context.Context, api githubAPI, r Release, dir string) error {
	if r.Draft || !r.Prerelease {
		return errors.New("selected alpha must be a published prerelease")
	}
	for _, name := range releaseAssets {
		var found []Asset
		for _, a := range r.Assets {
			if a.Name == name {
				found = append(found, a)
			}
		}
		if len(found) != 1 || found[0].ID <= 0 {
			return fmt.Errorf("alpha release asset missing or duplicated: %s", name)
		}
		b, e := api.call(ctx, "GET", endpoint(fmt.Sprintf("releases/assets/%d", found[0].ID)), nil)
		if e != nil {
			return e
		}
		if e := os.WriteFile(filepath.Join(dir, name), b, 0644); e != nil {
			return e
		}
	}
	return nil
}
func verifyTag(ctx context.Context, api githubAPI, tag, sha string) error {
	ref, e := getJSON[Ref](ctx, api, endpoint("git/ref/tags/"+tag))
	if e != nil {
		return e
	}
	if ref.Object.Type != "commit" || ref.Object.SHA != sha {
		return errors.New("release tag source mismatch")
	}
	return nil
}
func (p publication) publish(ctx context.Context, in publishInput, dir string) error {
	alpha := in.Channel == "alpha"
	if !alpha && in.Channel != "stable" {
		return errors.New("invalid release channel")
	}
	if _, e := parseVersion(in.Version, alpha); e != nil {
		return e
	}
	if strings.TrimSpace(in.Notes) == "" {
		return errors.New("human release notes are required")
	}
	if alpha && (strings.TrimSpace(in.Breaking) == "" || strings.TrimSpace(in.Upgrades) == "") {
		return errors.New("explicit breaking-change and upgrade declarations required (None is allowed)")
	}
	releases, e := pages[Release](ctx, p.api, endpoint("releases"), "")
	if e != nil {
		return e
	}
	tags, e := pages[Tag](ctx, p.api, endpoint("tags"), "")
	if e != nil {
		return e
	}
	if e := os.Mkdir(dir, 0755); e != nil {
		return e
	}
	var m Manifest
	var metadata Designation
	if alpha {
		id, e := strconv.ParseInt(in.SourceRun, 10, 64)
		if e != nil || id <= 0 || strconv.FormatInt(id, 10) != in.SourceRun {
			return errors.New("exact source CI run ID required")
		}
		run, e := getJSON[Run](ctx, p.api, endpoint("actions/runs/"+in.SourceRun))
		if e != nil {
			return e
		}
		if run.ID != id {
			return errors.New("source run selector mismatch")
		}
		if e := downloadCandidate(ctx, p.api, run, dir); e != nil {
			return e
		}
		m, _, e = verify(dir)
		if e != nil {
			return e
		}
		if e := validateSource(ctx, p.api, run, m); e != nil {
			return e
		}
		metadata = Designation{Version: in.Version, Channel: in.Channel, Breaking: strings.TrimSpace(in.Breaking), Upgrades: strings.TrimSpace(in.Upgrades)}
	} else {
		if _, e := parseVersion(in.Alpha, true); e != nil {
			return e
		}
		if in.Version != strings.Split(in.Alpha, "-alpha.")[0] {
			return errors.New("stable target must exactly remove selected alpha suffix")
		}
		release, e := getJSON[Release](ctx, p.api, endpoint("releases/tags/v"+in.Alpha))
		if e != nil {
			return e
		}
		if release.TagName != "v"+in.Alpha {
			return errors.New("selected alpha release tag mismatch")
		}
		if e := downloadRelease(ctx, p.api, release, dir); e != nil {
			return e
		}
		m, _, e = verify(dir)
		if e != nil {
			return e
		}
		if len(m.Qualification) != len(requiredJobs) {
			return errors.New("retained qualification evidence missing")
		}
		for _, n := range requiredJobs {
			if m.Qualification[n] <= 0 {
				return errors.New("retained qualification evidence missing")
			}
		}
		data, e := readAsset(dir, "release.json")
		if e != nil {
			return e
		}
		if e := strictJSON(data, &metadata); e != nil {
			return e
		}
		if metadata.Version != in.Alpha || metadata.Channel != "alpha" || metadata.Commit != m.Commit || metadata.PayloadSHA256 != m.PayloadSHA256 || strings.TrimSpace(metadata.Breaking) == "" || strings.TrimSpace(metadata.Upgrades) == "" {
			return errors.New("alpha release metadata mismatch")
		}
		if e := verifyTag(ctx, p.api, "v"+in.Alpha, m.Commit); e != nil {
			return e
		}
		metadata.Version = in.Version
		metadata.Channel = in.Channel
		metadata.PromotedFrom = in.Alpha
	}
	if e := progression(in.Version, alpha, !strings.EqualFold(metadata.Breaking, "None"), releases, tags); e != nil {
		return e
	}
	metadata.Commit = m.Commit
	metadata.PayloadSHA256 = m.PayloadSHA256
	if e := writeJSON(dir, "release.json", metadata); e != nil {
		return e
	}
	tag := "v" + in.Version
	// The approved release App is used exclusively for this immutable ref write.
	// Never fall back to the workflow token, a different commit, or a forced ref.
	if e := p.createTag(ctx, tag, m.Commit); e != nil {
		return e
	}
	body := in.Notes + "\n\n## Breaking changes\n" + metadata.Breaking + "\n\n## Upgrade requirements\n" + metadata.Upgrades + fmt.Sprintf("\n\nSource: %s (CI run %d, attempt %d).\nPre-1.0: recommended does not mean production-ready or permanently backward-compatible.", m.Commit, m.RunID, m.Attempt)
	b, e := p.api.call(ctx, "POST", endpoint("releases"), Release{TagName: tag, Target: m.Commit, Name: tag, Body: body, Draft: true, Prerelease: alpha})
	if e != nil {
		return e
	}
	var draft Release
	if e := json.Unmarshal(b, &draft); e != nil {
		return e
	}
	if draft.ID <= 0 {
		return errors.New("missing draft release identity")
	}
	files := []string{}
	for _, n := range releaseAssets {
		files = append(files, filepath.Join(dir, n))
	}
	if e := p.upload(ctx, tag, files); e != nil {
		return e
	}
	attached, e := getJSON[Release](ctx, p.api, endpoint(fmt.Sprintf("releases/%d", draft.ID)))
	if e != nil {
		return e
	}
	if len(attached.Assets) != len(releaseAssets) || !attached.Draft || attached.TagName != tag || attached.Prerelease != alpha {
		return errors.New("draft release identity/asset set mismatch")
	}
	seen := map[string]bool{}
	for _, a := range attached.Assets {
		allowed := false
		for _, n := range releaseAssets {
			if a.Name == n {
				allowed = true
			}
		}
		if !allowed || seen[a.Name] || a.ID <= 0 {
			return errors.New("draft asset set mismatch")
		}
		seen[a.Name] = true
		actual, e := p.api.call(ctx, "GET", endpoint(fmt.Sprintf("releases/assets/%d", a.ID)), nil)
		if e != nil {
			return e
		}
		expected, e := readAsset(dir, a.Name)
		if e != nil {
			return e
		}
		if !bytes.Equal(actual, expected) {
			return errors.New("uploaded asset bytes differ")
		}
	}
	if e := verifyTag(ctx, p.api, tag, m.Commit); e != nil {
		return e
	}
	latest := "true"
	if alpha {
		latest = "false"
	}
	_, e = p.api.call(ctx, "PATCH", endpoint(fmt.Sprintf("releases/%d", draft.ID)), struct {
		Draft      bool   `json:"draft"`
		MakeLatest string `json:"make_latest"`
	}{false, latest})
	return e
}
