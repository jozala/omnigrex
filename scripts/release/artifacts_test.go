package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func mustWrite(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(dir, name), b, 0644); e != nil {
		t.Fatal(e)
	}
}
func fixtureBuild() Build {
	b := Build{SchemaVersion: 1, Repository: repository, Commit: strings.Repeat("a", 40), RunID: 17, Attempt: 2, Platform: "linux/amd64", Images: map[string]string{}}
	for i, n := range products {
		b.Images[n] = "ghcr.io/jozala/omnigrex/" + n + "@sha256:" + strings.Repeat(string(rune('0'+i)), 64)
	}
	return b
}
func packed(t *testing.T) (string, Manifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "candidate")
	if e := pack(dir, filepath.Join("..", ".."), fixtureBuild()); e != nil {
		t.Fatal(e)
	}
	m, _, e := verify(dir)
	if e != nil {
		t.Fatal(e)
	}
	return dir, m
}
func evidence() map[string]int64 {
	m := map[string]int64{}
	for i, n := range requiredJobs {
		m[n] = int64(i + 1)
	}
	return m
}
func qualified(t *testing.T) (string, Manifest) {
	t.Helper()
	dir, m := packed(t)
	m.Qualification = evidence()
	if e := writeJSON(dir, "candidate.json", m); e != nil {
		t.Fatal(e)
	}
	if e := writeChecksums(dir); e != nil {
		t.Fatal(e)
	}
	return dir, m
}
func assertError(t *testing.T, e error, contains string) {
	t.Helper()
	if e == nil || !strings.Contains(e.Error(), contains) {
		t.Fatalf("error = %v, want containing %q", e, contains)
	}
}
func TestPackVerifyNoCheckoutCompose(t *testing.T) {
	dir, m := packed(t)
	_, files, e := verify(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(files) != 5 || len(m.Images) != 3 || m.Images["development"] != "" {
		t.Fatal("unexpected product artifact set")
	}
	root := t.TempDir()
	if e := os.Mkdir(filepath.Join(root, "deploy"), 0755); e != nil {
		t.Fatal(e)
	}
	for n, data := range files {
		mustWrite(t, root, n, data)
	}
	env := append(os.Environ(), "OMNIGREX_PI_IMAGE="+m.Images["pi"], "OMNIGREX_PI_PLATFORM=linux/amd64", "OMNIGREX_DOCKER_GID=0", "OMNIGREX_SECRET_GID=1", "OMNIGREX_MICROSANDBOX_KVM_GID=2", "OMNIGREX_GITHUB_DEVELOPER_APP_ID=1", "OMNIGREX_GITHUB_REVIEWER_APP_ID=2")
	for _, n := range []string{"DATABASE_PASSWORD", "GITHUB_DEVELOPER_PRIVATE_KEY", "GITHUB_REVIEWER_PRIVATE_KEY", "GITHUB_WEBHOOK_SECRET", "PROVIDER_CREDENTIALS"} {
		env = append(env, "OMNIGREX_"+n+"_FILE=/dev/null")
	}
	for _, backend := range []string{"docker", "microsandbox"} {
		controller := "orchestrator"
		if backend == "microsandbox" {
			controller += "-microsandbox"
		}
		cmd := exec.Command("docker", "compose", "-f", "compose.pi.yaml", "-f", "compose.pi-"+backend+".yaml", "config", "--quiet")
		cmd.Dir = filepath.Join(root, "deploy")
		cmd.Env = append(env, "OMNIGREX_ORCHESTRATOR_IMAGE="+m.Images[controller])
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("no-checkout %s configuration: %v %s", backend, e, out)
		}
	}
}
func TestPackExcludesInstallationSecrets(t *testing.T) {
	source := t.TempDir()
	if e := os.MkdirAll(filepath.Join(source, "deploy", "secrets"), 0755); e != nil {
		t.Fatal(e)
	}
	for _, n := range deploymentFiles {
		b, e := os.ReadFile(filepath.Join("..", "..", "deploy", n))
		if e != nil {
			t.Fatal(e)
		}
		mustWrite(t, source, "deploy/"+n, b)
	}
	for _, n := range []string{".env", "secrets/key", "compatibility.json", "backup.tar", "private-installation.json"} {
		mustWrite(t, source, "deploy/"+n, []byte("private-test-sentinel"))
	}
	dir := filepath.Join(t.TempDir(), "candidate")
	if e := pack(dir, source, fixtureBuild()); e != nil {
		t.Fatal(e)
	}
	_, files, e := verify(dir)
	if e != nil {
		t.Fatal(e)
	}
	for n, b := range files {
		if bytes.Contains(b, []byte("private-test-sentinel")) {
			t.Fatalf("secret included: %s", n)
		}
	}
}
func TestTamperingAndIdentityMismatch(t *testing.T) {
	for _, name := range candidateAssets {
		t.Run(name, func(t *testing.T) {
			dir, _ := packed(t)
			b, e := readAsset(dir, name)
			if e != nil {
				t.Fatal(e)
			}
			mustWrite(t, dir, name, append(b, []byte("bad")...))
			if _, _, e := verify(dir); e == nil {
				t.Fatal("tampered asset accepted")
			}
		})
	}
	for _, kind := range []string{"digest", "commit", "run", "attempt", "platform", "repository", "development", "schema", "files"} {
		t.Run(kind, func(t *testing.T) {
			dir, m := packed(t)
			switch kind {
			case "digest":
				m.Images["pi"] = "ghcr.io/jozala/omnigrex/pi@sha256:" + strings.Repeat("f", 64)
			case "commit":
				m.Commit = strings.Repeat("b", 40)
			case "run":
				m.RunID++
			case "attempt":
				m.Attempt++
			case "platform":
				m.Platform = "linux/arm64"
			case "repository":
				m.Repository = "fork/omnigrex"
			case "development":
				m.Images["development"] = m.Images["pi"]
			case "schema":
				m.SchemaVersion = 2
			case "files":
				m.Files["deploy/.env"] = "bad"
			}
			if e := writeJSON(dir, "candidate.json", m); e != nil {
				t.Fatal(e)
			}
			if e := writeChecksums(dir); e != nil {
				t.Fatal(e)
			}
			if _, _, e := verify(dir); e == nil {
				t.Fatal("manifest mismatch accepted")
			}
		})
	}
}
func TestUnsafeArchivesRejectedEvenWithMatchingChecksums(t *testing.T) {
	for _, kind := range []string{"traversal", "secret", "symlink", "duplicate", "missing", "mode", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			dir, m := packed(t)
			_, files, e := verify(dir)
			if e != nil {
				t.Fatal(e)
			}
			var payload bytes.Buffer
			gz := gzip.NewWriter(&payload)
			tw := tar.NewWriter(gz)
			put := func(h *tar.Header, b []byte) {
				t.Helper()
				if e := tw.WriteHeader(h); e != nil {
					t.Fatal(e)
				}
				if _, e := tw.Write(b); e != nil {
					t.Fatal(e)
				}
			}
			for n, b := range files {
				if kind == "missing" && n == "build.json" {
					continue
				}
				put(&tar.Header{Name: n, Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(b))}, b)
			}
			h := &tar.Header{Name: "deploy/compose.pi.yaml", Mode: 0644, Typeflag: tar.TypeReg}
			switch kind {
			case "traversal":
				h.Name = "../escape"
			case "secret":
				h.Name = "deploy/secrets/key"
			case "symlink":
				h.Typeflag = tar.TypeSymlink
				h.Linkname = "/tmp/escape"
			case "mode":
				h.Mode = 04755
			case "oversize":
				h.Size = 1<<20 + 1
			}
			if kind != "missing" {
				var b []byte
				if h.Size > 0 {
					b = make([]byte, h.Size)
				}
				put(h, b)
			}
			if e := tw.Close(); e != nil {
				t.Fatal(e)
			}
			if e := gz.Close(); e != nil {
				t.Fatal(e)
			}
			mustWrite(t, dir, "payload.tar.gz", payload.Bytes())
			m.PayloadSHA256 = sum(payload.Bytes())
			if e := writeJSON(dir, "candidate.json", m); e != nil {
				t.Fatal(e)
			}
			if e := writeChecksums(dir); e != nil {
				t.Fatal(e)
			}
			if _, _, e := verify(dir); e == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestReadinessRequiresExactIdentityAndSuccessfulEvidence(t *testing.T) {
	dir, m := packed(t)
	api := apiFunc(func(ctx context.Context, method, path string, body any) ([]byte, error) {
		return jobsJSON(t, m.Build, allJobs(m.Build)), nil
	})
	before, _ := readAsset(dir, "payload.tar.gz")
	if e := ready(context.Background(), api, dir, m.Build); e != nil {
		t.Fatal(e)
	}
	actual, _, e := verify(dir)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := readAsset(dir, "payload.tar.gz")
	if !bytes.Equal(before, after) || !reflect.DeepEqual(actual.Qualification, evidence()) {
		t.Fatal("readiness changed build or lost evidence")
	}
	identity := m.Build
	identity.Attempt++
	assertError(t, ready(context.Background(), api, dir, identity), "identity mismatch")
	empty := apiFunc(func(context.Context, string, string, any) ([]byte, error) { return []byte(`{"jobs":[]}`), nil })
	original, _ := readAsset(dir, "candidate.json")
	assertError(t, ready(context.Background(), empty, dir, m.Build), "qualification")
	current, _ := readAsset(dir, "candidate.json")
	if !bytes.Equal(original, current) {
		t.Fatal("failed qualification changed readiness evidence")
	}
}

func TestCommandPackAndVerifyUseActualCIEnvironment(t *testing.T) {
	b := fixtureBuild()
	root := t.TempDir()
	if e := os.Mkdir(filepath.Join(root, "deploy"), 0755); e != nil {
		t.Fatal(e)
	}
	for _, n := range deploymentFiles {
		data, e := os.ReadFile(filepath.Join("..", "..", "deploy", n))
		if e != nil {
			t.Fatal(e)
		}
		mustWrite(t, root, "deploy/"+n, data)
	}
	t.Chdir(root)
	for k, v := range map[string]string{"GITHUB_REPOSITORY": repository, "GITHUB_SHA": b.Commit, "GITHUB_RUN_ID": "17", "GITHUB_RUN_ATTEMPT": "2"} {
		t.Setenv(k, v)
	}
	for _, n := range products {
		t.Setenv(strings.ToUpper(strings.ReplaceAll(n, "-", "_"))+"_IMAGE", b.Images[n])
	}
	if e := execute(context.Background(), []string{"pack", "candidate"}); e != nil {
		t.Fatal(e)
	}
	if e := execute(context.Background(), []string{"verify", "candidate"}); e != nil {
		t.Fatal(e)
	}
	m, _, e := verify("candidate")
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(m.Build, b) {
		t.Fatal("CI environment identity lost")
	}
}
func TestGzipFooterCorruptionIsRejectedWithSelfConsistentOuterChecksums(t *testing.T) {
	dir, m := packed(t)
	b, e := readAsset(dir, "payload.tar.gz")
	if e != nil {
		t.Fatal(e)
	}
	b[len(b)-1] ^= 1
	mustWrite(t, dir, "payload.tar.gz", b)
	m.PayloadSHA256 = sum(b)
	if e := writeJSON(dir, "candidate.json", m); e != nil {
		t.Fatal(e)
	}
	if e := writeChecksums(dir); e != nil {
		t.Fatal(e)
	}
	if _, _, e := verify(dir); e == nil {
		t.Fatal("malformed gzip accepted")
	}
}
