package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type apiFunc func(context.Context, string, string, any) ([]byte, error)

func (f apiFunc) call(ctx context.Context, m, p string, b any) ([]byte, error) {
	return f(ctx, m, p, b)
}
func allJobs(b Build) []Job {
	var out []Job
	for i, n := range append(append([]string{}, requiredJobs...), "release-candidate") {
		out = append(out, Job{ID: int64(i + 1), Name: n, Status: "completed", Conclusion: "success", RunID: b.RunID, Attempt: b.Attempt, Commit: b.Commit})
	}
	return out
}
func jobsJSON(t *testing.T, b Build, jobs []Job) []byte {
	t.Helper()
	return mustJSON(t, struct {
		Jobs []Job `json:"jobs"`
	}{jobs})
}
func fixtureRun(b Build) Run {
	return Run{ID: b.RunID, Attempt: b.Attempt, Commit: b.Commit, Repository: Repo{repository}, HeadRepository: Repo{repository}, Event: "push", Branch: "main", Path: ".github/workflows/ci.yaml", WorkflowID: 12, Status: "completed", Conclusion: "success"}
}
func sourceAPI(t *testing.T, b Build, jobs []Job) githubAPI {
	return apiFunc(func(ctx context.Context, method, path string, body any) ([]byte, error) {
		switch {
		case strings.HasSuffix(path, "actions/workflows/ci.yaml"):
			return []byte(`{"id":12}`), nil
		case strings.Contains(path, "compare/"):
			return []byte(`{"status":"ahead"}`), nil
		case strings.Contains(path, "/jobs?"):
			return jobsJSON(t, b, jobs), nil
		default:
			return nil, fmt.Errorf("unexpected source API request: %s", path)
		}
	})
}
func TestSourceProvenance(t *testing.T) {
	_, m := qualified(t)
	run := fixtureRun(m.Build)
	api := sourceAPI(t, m.Build, allJobs(m.Build))
	if e := validateSource(context.Background(), api, run, m); e != nil {
		t.Fatal(e)
	}
	cases := map[string]func(*Run){"event": func(r *Run) { r.Event = "pull_request" }, "branch": func(r *Run) { r.Branch = "other" }, "head repository": func(r *Run) { r.HeadRepository.FullName = "fork/omnigrex" }, "repository": func(r *Run) { r.Repository.FullName = "fork/omnigrex" }, "path": func(r *Run) { r.Path = ".github/workflows/other.yaml" }, "workflow": func(r *Run) { r.WorkflowID = 99 }, "conclusion": func(r *Run) { r.Conclusion = "failure" }, "pending": func(r *Run) { r.Status = "in_progress" }, "commit": func(r *Run) { r.Commit = strings.Repeat("b", 40) }, "attempt": func(r *Run) { r.Attempt++ }, "id": func(r *Run) { r.ID++ }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := run
			change(&changed)
			if e := validateSource(context.Background(), api, changed, m); e == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	m.Qualification["check"] = 999
	assertError(t, validateSource(context.Background(), api, run, m), "job identity")
	diverged := apiFunc(func(ctx context.Context, method, path string, body any) ([]byte, error) {
		if strings.Contains(path, "compare/") {
			return []byte(`{"status":"diverged"}`), nil
		}
		return api.call(ctx, method, path, body)
	})
	assertError(t, validateSource(context.Background(), diverged, run, m), "ancestor")
}
func TestQualificationFailsClosed(t *testing.T) {
	b := fixtureBuild()
	for _, name := range append(append([]string{}, requiredJobs...), "release-candidate") {
		for _, state := range []string{"failure", "cancelled", "skipped", "pending", "", "missing", "duplicate", "wrong run", "wrong attempt", "wrong commit"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				jobs := allJobs(b)
				for i := range jobs {
					if jobs[i].Name != name {
						continue
					}
					switch state {
					case "missing":
						jobs = append(jobs[:i], jobs[i+1:]...)
					case "duplicate":
						jobs = append(jobs, jobs[i])
					case "wrong run":
						jobs[i].RunID++
					case "wrong attempt":
						jobs[i].Attempt++
					case "wrong commit":
						jobs[i].Commit = strings.Repeat("f", 40)
					case "pending":
						jobs[i].Status = "in_progress"
					default:
						jobs[i].Conclusion = state
					}
					break
				}
				_, e := qualification(context.Background(), sourceAPI(t, b, jobs), b, []string{name})
				if e == nil {
					t.Fatal("invalid evidence accepted")
				}
			})
		}
	}
	unavailable := apiFunc(func(context.Context, string, string, any) ([]byte, error) {
		return nil, errors.New("evidence unavailable")
	})
	_, e := qualification(context.Background(), unavailable, b, requiredJobs)
	assertError(t, e, "unavailable")
}
func zipCandidate(t *testing.T, dir string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, n := range candidateAssets {
		w, e := z.Create(n)
		if e != nil {
			t.Fatal(e)
		}
		data, e := readAsset(dir, n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := w.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestArtifactAvailabilityAndMalformedPaths(t *testing.T) {
	run := fixtureRun(fixtureBuild())
	for _, kind := range []string{"missing", "expired", "ambiguous", "wrong attempt", "unavailable", "malformed", "path"} {
		t.Run(kind, func(t *testing.T) {
			a := Artifact{ID: 8, Name: "release-candidate-17-2"}
			artifacts := []Artifact{a}
			switch kind {
			case "missing":
				artifacts = nil
			case "expired":
				artifacts[0].Expired = true
			case "ambiguous":
				artifacts = append(artifacts, a)
			case "wrong attempt":
				artifacts[0].Name = "release-candidate-17-1"
			}
			api := apiFunc(func(ctx context.Context, method, path string, body any) ([]byte, error) {
				if strings.Contains(path, "/artifacts?") {
					return mustJSON(t, struct {
						Artifacts []Artifact `json:"artifacts"`
					}{artifacts}), nil
				}
				if kind == "unavailable" {
					return nil, errors.New("artifact unavailable")
				}
				if kind == "path" {
					var b bytes.Buffer
					z := zip.NewWriter(&b)
					for _, n := range []string{"../secret", "candidate.json", "SHA256SUMS"} {
						w, _ := z.Create(n)
						_, _ = w.Write([]byte("x"))
					}
					_ = z.Close()
					return b.Bytes(), nil
				}
				return []byte("not zip"), nil
			})
			if e := downloadCandidate(context.Background(), api, run, t.TempDir()); e == nil {
				t.Fatal("invalid artifact accepted")
			}
		})
	}
}

// This in-memory GitHub boundary models the maintainer-reported authorization
// rejection: the workflow token cannot create the historical ref. Only the
// dedicated App token with requested Contents/Workflows write is allowed.
// This is a fixture, not proof of live GitHub permission behavior.
type githubFixture struct {
	t                                             *testing.T
	build                                         Build
	candidateZip                                  []byte
	releases                                      []Release
	drafts                                        map[int64]Release
	assets                                        map[int64][]byte
	tags                                          map[string]string
	requests                                      []string
	failUpload, corruptUpload, denyTag, ciExpired bool
	appMinted, appRevoked, appTags                int
	key                                           string
	now                                           time.Time
}

func newGitHubFixture(t *testing.T) *githubFixture {
	t.Helper()
	dir, m := qualified(t)
	return &githubFixture{t: t, build: m.Build, candidateZip: zipCandidate(t, dir), releases: []Release{}, drafts: map[int64]Release{}, assets: map[int64][]byte{}, tags: map[string]string{}, key: testPrivateKey(t), now: time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)}
}
func (f *githubFixture) workflowAPI() githubAPI { return apiFunc(f.workflowCall) }
func (f *githubFixture) workflowCall(ctx context.Context, method, path string, body any) ([]byte, error) {
	f.requests = append(f.requests, method+" "+path)
	tail := strings.TrimPrefix(path, endpoint(""))
	if strings.HasPrefix(tail, "actions/") && f.ciExpired {
		return nil, errors.New("CI artifacts and evidence expired")
	}
	switch {
	case strings.HasPrefix(tail, "releases?"):
		return mustJSON(f.t, f.releases), nil
	case strings.HasPrefix(tail, "tags?"):
		tags := []Tag{}
		for n := range f.tags {
			tags = append(tags, Tag{n})
		}
		return mustJSON(f.t, tags), nil
	case tail == "actions/runs/17":
		return mustJSON(f.t, fixtureRun(f.build)), nil
	case strings.HasPrefix(tail, "actions/runs/17/artifacts?"):
		return []byte(`{"artifacts":[{"id":8,"name":"release-candidate-17-2","expired":false}]}`), nil
	case tail == "actions/artifacts/8/zip":
		return f.candidateZip, nil
	case tail == "git/refs":
		return nil, errors.New("GitHub POST git/refs returned HTTP 403: workflow token lacks historical workflows-write authority")
	case tail == "releases" && method == "POST":
		var r Release
		if e := json.Unmarshal(mustJSON(f.t, body), &r); e != nil {
			f.t.Fatal(e)
		}
		r.ID = int64(len(f.drafts) + 1)
		f.drafts[r.ID] = r
		return mustJSON(f.t, r), nil
	case strings.HasPrefix(tail, "releases/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(tail, "releases/assets/"), 10, 64)
		b := f.assets[id]
		if f.corruptUpload {
			b = append(append([]byte{}, b...), "corrupt"...)
		}
		return b, nil
	case strings.HasPrefix(tail, "releases/tags/"):
		for _, r := range f.releases {
			if r.TagName == strings.TrimPrefix(tail, "releases/tags/") {
				return mustJSON(f.t, r), nil
			}
		}
		return nil, errors.New("release missing")
	case strings.HasPrefix(tail, "git/ref/tags/"):
		var r Ref
		r.Object.Type = "commit"
		r.Object.SHA = f.tags[strings.TrimPrefix(tail, "git/ref/tags/")]
		return mustJSON(f.t, r), nil
	case strings.HasPrefix(tail, "releases/"):
		id, e := strconv.ParseInt(strings.TrimPrefix(tail, "releases/"), 10, 64)
		if e != nil {
			return nil, e
		}
		r := f.drafts[id]
		if method == "PATCH" {
			var patch struct {
				Draft      bool   `json:"draft"`
				MakeLatest string `json:"make_latest"`
			}
			if e := json.Unmarshal(mustJSON(f.t, body), &patch); e != nil {
				return nil, e
			}
			if patch.MakeLatest != map[bool]string{true: "false", false: "true"}[r.Prerelease] {
				f.t.Fatal("incorrect recommended-release discovery")
			}
			r.Draft = patch.Draft
			f.drafts[id] = r
			f.releases = append(f.releases, r)
		}
		return mustJSON(f.t, r), nil
	default:
		return sourceAPI(f.t, f.build, allJobs(f.build)).call(ctx, method, path, body)
	}
}
func (f *githubFixture) tokenAPI(token string) githubAPI {
	if token == "installation-token-fixture" {
		return apiFunc(func(ctx context.Context, method, path string, body any) ([]byte, error) {
			if method == "DELETE" && path == "installation/token" {
				f.appRevoked++
				return nil, nil
			}
			if method != "POST" || path != endpoint("git/refs") {
				return nil, fmt.Errorf("App installation token used beyond tag creation: %s %s", method, path)
			}
			if f.denyTag {
				return nil, errors.New("GitHub POST git/refs returned HTTP 403")
			}
			var ref struct{ Ref, SHA string }
			if e := json.Unmarshal(mustJSON(f.t, body), &ref); e != nil {
				f.t.Fatal(e)
			}
			name := strings.TrimPrefix(ref.Ref, "refs/tags/")
			if f.tags[name] != "" {
				return nil, errors.New("ref already exists")
			}
			if ref.SHA != f.build.Commit {
				f.t.Fatal("selected historical commit was substituted")
			}
			f.tags[name] = ref.SHA
			f.appTags++
			return []byte(`{}`), nil
		})
	}
	verifyJWT(f.t, token, f.key, f.now)
	return apiFunc(func(ctx context.Context, method, path string, body any) ([]byte, error) {
		if method == "GET" && path == endpoint("installation") {
			return []byte(`{"id":41}`), nil
		}
		if method == "POST" && path == "app/installations/41/access_tokens" {
			var request struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if e := json.Unmarshal(mustJSON(f.t, body), &request); e != nil {
				f.t.Fatal(e)
			}
			if !reflect.DeepEqual(request.Repositories, []string{"omnigrex"}) || !reflect.DeepEqual(request.Permissions, map[string]string{"contents": "write", "workflows": "write"}) {
				f.t.Fatal("App token not scoped to approved repository/permissions")
			}
			f.appMinted++
			return mustJSON(f.t, map[string]any{"token": "installation-token-fixture", "expires_at": f.now.Add(time.Hour), "permissions": map[string]string{"contents": "write", "workflows": "write", "metadata": "read"}, "repositories": []Repo{{repository}}}), nil
		}
		return nil, fmt.Errorf("unexpected App JWT request %s %s", method, path)
	})
}
func (f *githubFixture) publication() publication {
	tagger := appTagger{appID: "123", privateKey: f.key, apiForToken: f.tokenAPI, now: func() time.Time { return f.now }}
	return publication{api: f.workflowAPI(), createTag: tagger.create, upload: func(ctx context.Context, tag string, files []string) error {
		if f.failUpload {
			return errors.New("upload failed")
		}
		id := int64(len(f.drafts))
		r := f.drafts[id]
		if r.TagName != tag {
			return errors.New("unexpected upload tag")
		}
		for _, p := range files {
			b, e := os.ReadFile(p)
			if e != nil {
				return e
			}
			assetID := int64(len(f.assets) + 1)
			f.assets[assetID] = b
			r.Assets = append(r.Assets, Asset{assetID, filepath.Base(p)})
		}
		f.drafts[id] = r
		return nil
	}}
}
func alphaInput() publishInput {
	return publishInput{Channel: "alpha", Version: "0.1.0-alpha.1", Notes: "Human notes", Breaking: "None", Upgrades: "Fresh install; historical upgrades unqualified", SourceRun: "17"}
}
func TestHistoricalPublicationRedesignationAndNoRebuildPromotion(t *testing.T) {
	f := newGitHubFixture(t)
	p := f.publication()
	ctx := context.Background()
	in := alphaInput()
	// The very same historical target fails with the ordinary workflow token.
	_, e := f.workflowCall(ctx, "POST", endpoint("git/refs"), map[string]string{"ref": "refs/tags/v0.1.0-alpha.1", "sha": f.build.Commit})
	assertError(t, e, "403")
	alpha := filepath.Join(t.TempDir(), "alpha")
	if e := p.publish(ctx, in, alpha); e != nil {
		t.Fatal(e)
	}
	original := mustJSON(t, f.releases[0])
	f.requests = nil
	in.Version = "0.2.0-alpha.1"
	in.Breaking = "New storage format"
	redesignated := filepath.Join(t.TempDir(), "redesignated")
	if e := p.publish(ctx, in, redesignated); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(original, mustJSON(t, f.releases[0])) {
		t.Fatal("earlier release changed")
	}
	for _, n := range candidateAssets {
		a, _ := readAsset(alpha, n)
		b, _ := readAsset(redesignated, n)
		if !bytes.Equal(a, b) {
			t.Fatal("redesignation changed artifact", n)
		}
	}
	assertError(t, p.publish(ctx, in, filepath.Join(t.TempDir(), "duplicate")), "already exists")
	stableInput := publishInput{Channel: "stable", Version: "0.3.0", Alpha: in.Version, Notes: "Finalized human notes"}
	assertError(t, p.publish(ctx, stableInput, filepath.Join(t.TempDir(), "mismatch")), "exactly remove")
	f.ciExpired = true
	f.requests = nil
	stableInput.Version = "0.2.0"
	stable := filepath.Join(t.TempDir(), "stable")
	if e := p.publish(ctx, stableInput, stable); e != nil {
		t.Fatal(e)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "actions/") {
			t.Fatal("promotion depended on expired CI", r)
		}
	}
	for _, n := range candidateAssets {
		a, _ := readAsset(redesignated, n)
		b, _ := readAsset(stable, n)
		if !bytes.Equal(a, b) {
			t.Fatal("promotion changed tested bytes/checksums", n)
		}
	}
	m, _, e := verify(stable)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(m.Images, f.build.Images) {
		t.Fatal("promotion changed digests")
	}
	if f.tags["v0.2.0"] != f.tags["v0.2.0-alpha.1"] || f.releases[2].Prerelease || f.releases[2].Draft || f.appTags != 3 || f.appMinted != 3 || f.appRevoked != 3 {
		t.Fatal("incorrect promotion or App token lifecycle")
	}
}
func TestPublicationFailuresNeverPublishOrFallback(t *testing.T) {
	for _, failure := range []string{"tag authorization", "upload", "verification", "missing alpha asset", "alpha tag mismatch", "alpha metadata mismatch"} {
		t.Run(failure, func(t *testing.T) {
			f := newGitHubFixture(t)
			p := f.publication()
			in := alphaInput()
			switch failure {
			case "tag authorization":
				f.denyTag = true
			case "upload":
				f.failUpload = true
			case "verification":
				f.corruptUpload = true
			default:
				if e := p.publish(context.Background(), in, filepath.Join(t.TempDir(), "alpha")); e != nil {
					t.Fatal(e)
				}
				in = publishInput{Channel: "stable", Version: "0.1.0", Alpha: "0.1.0-alpha.1", Notes: "Stable notes"}
				switch failure {
				case "missing alpha asset":
					f.releases[0].Assets = f.releases[0].Assets[:3]
				case "alpha tag mismatch":
					f.tags["v0.1.0-alpha.1"] = strings.Repeat("f", 40)
				case "alpha metadata mismatch":
					for _, a := range f.releases[0].Assets {
						if a.Name == "release.json" {
							f.assets[a.ID] = []byte(`{"version":"0.9.0-alpha.1"}`)
						}
					}
				}
			}
			before := len(f.releases)
			if e := p.publish(context.Background(), in, filepath.Join(t.TempDir(), "failed")); e == nil {
				t.Fatal("failed publication accepted")
			}
			if len(f.releases) != before {
				t.Fatal("failure published a release")
			}
			if failure == "tag authorization" {
				if len(f.drafts) != 0 || len(f.tags) != 0 || f.appRevoked != 1 {
					t.Fatal("tag denial created draft/tag or retained token")
				}
			} else if failure == "upload" || failure == "verification" {
				if !f.drafts[1].Draft || f.appRevoked != 1 {
					t.Fatal("failure must leave only an unpublished draft and revoke token")
				}
			}
			for _, request := range f.requests {
				if request == "POST "+endpoint("git/refs") {
					t.Fatal("fell back to workflow token")
				}
			}
		})
	}
}
func TestExplicitDeclarationsRequired(t *testing.T) {
	for _, field := range []string{"notes", "breaking", "upgrades"} {
		f := newGitHubFixture(t)
		in := alphaInput()
		switch field {
		case "notes":
			in.Notes = " "
		case "breaking":
			in.Breaking = ""
		case "upgrades":
			in.Upgrades = " "
		}
		if e := f.publication().publish(context.Background(), in, filepath.Join(t.TempDir(), "release")); e == nil {
			t.Fatal("implicit declaration accepted")
		}
		if f.appMinted != 0 {
			t.Fatal("credential minted before input validation")
		}
	}
}
