package development_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// These tests run in the disposable cache-qualification image with no external
// network and a read-only root filesystem. Normal repository tests skip them.
func cacheEnvironment(t *testing.T) []string {
	t.Helper()
	if os.Getenv("OMNIGREX_DEVELOPMENT_CACHE_TEST") != "1" {
		t.Skip("run mise run test-development-image-cache")
	}
	root := t.TempDir()
	environment := []string{
		"PATH=/opt/qualification/go/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=" + root, "TMPDIR=" + root,
		"GOPATH=" + filepath.Join(root, "gopath"),
		"GOCACHE=" + filepath.Join(root, "build"),
		"GOTOOLCHAIN=local", "GOFLAGS=-p=1 -modcacherw",
		"MISE_DATA_DIR=" + filepath.Join(root, "mise"),
		"MISE_CONFIG_DIR=" + filepath.Join(root, "config"),
		"MISE_CACHE_DIR=" + filepath.Join(root, "cache"),
		"MISE_STATE_DIR=" + filepath.Join(root, "state"),
		"MISE_GLOBAL_CONFIG_FILE=/nonexistent", "MISE_SYSTEM_CONFIG_FILE=/nonexistent",
		"MISE_TRUSTED_CONFIG_PATHS=" + root,
	}
	// Go's telemetry sidecar can outlive a command and race TempDir cleanup.
	// Disable it in this disposable HOME before running any other Go commands.
	run(t, root, environment, "go", "telemetry", "off")
	return environment
}

func run(t *testing.T, directory string, environment []string, program string, args ...string) []byte {
	t.Helper()
	if program == "go" {
		program = "/opt/qualification/go/bin/go"
	}
	command := exec.CommandContext(t.Context(), program, args...)
	command.Dir, command.Env = directory, environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", program, args, err, output)
	}
	return output
}

func TestCacheEnvironmentDisablesGoTelemetry(t *testing.T) {
	environment := cacheEnvironment(t)
	output := run(t, t.TempDir(), environment, "go", "env", "GOTELEMETRY")
	if got := strings.TrimSpace(string(output)); got != "off" {
		t.Fatalf("Go telemetry mode = %q; want off", got)
	}
}

func TestProjectModulesComeFromImageAndRemainCached(t *testing.T) {
	environment := cacheEnvironment(t)
	module := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum"} {
		content, err := os.ReadFile("/opt/qualification/module/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(module, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	activation := t.TempDir()
	if err := os.WriteFile(filepath.Join(activation, "mise.toml"), []byte("[env]\n_.source = '/opt/qualification/development-environment.sh'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := json.Unmarshal(run(t, activation, append(environment, "MISE_TRUSTED_CONFIG_PATHS="+activation), "/usr/local/bin/mise", "env", "--json"), &values); err != nil {
		t.Fatal(err)
	}
	if values["GOPROXY"] != "file:///opt/omnigrex/go-proxy,https://proxy.golang.org,direct" {
		t.Fatalf("mise activation did not select the local module source: %q", values["GOPROXY"])
	}
	// No network is available: downloads selected by the checked-in manifests
	// must come from the image, with their existing go.sum hashes still checked.
	run(t, module, append(environment, "GOPROXY="+values["GOPROXY"]), "go", "mod", "download")
	// Disable even the local source to demonstrate reuse of the writable cache.
	run(t, module, append(environment, "GOPROXY=off"), "go", "mod", "download")
}

func TestMissingModuleFallsBackAndIsCached(t *testing.T) {
	environment := cacheEnvironment(t)
	const module = "cache.example/extra"
	const version = "v1.0.0"
	mod := []byte("module " + module + "\n\ngo 1.20\n")
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range map[string][]byte{"go.mod": mod, "extra.go": []byte("package extra\n")} {
		file, err := writer.Create(module + "@" + version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/" + module + "/@v/" + version + ".mod":
			_, _ = w.Write(mod)
		case "/" + module + "/@v/" + version + ".info":
			_, _ = w.Write([]byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`))
		case "/" + module + "/@v/" + version + ".zip":
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	directory := t.TempDir()
	// Only this synthetic module bypasses the public checksum database.
	environment = append(environment, "GONOSUMDB="+module)
	run(t, directory, append(environment, "GOPROXY=file:///opt/omnigrex/go-proxy,"+server.URL), "go", "mod", "download", module+"@"+version)
	if requests.Load() == 0 {
		t.Fatal("missing module did not reach the fallback proxy")
	}
	server.Close()
	run(t, directory, append(environment, "GOPROXY=off"), "go", "mod", "download", module+"@"+version)
	// A fresh Participant must not accidentally observe another one's cache.
	fresh := append(cacheEnvironment(t), "GOPROXY=off", "GONOSUMDB="+module)
	command := exec.CommandContext(t.Context(), "/opt/qualification/go/bin/go", "mod", "download", module+"@"+version)
	command.Env = fresh
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "GOPROXY=off") {
		t.Fatalf("fresh cache unexpectedly contains the module: %v\n%s", err, output)
	}
}

func TestMisePreservesUpstreamAndDoesNotDuplicateLocalSource(t *testing.T) {
	environment := cacheEnvironment(t)
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "mise.toml"), []byte("[env]\n_.source = '/opt/qualification/development-environment.sh'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	environment = append(environment, "MISE_TRUSTED_CONFIG_PATHS="+directory)
	const upstream = "https://proxy.example.invalid,direct"
	const want = "file:///opt/omnigrex/go-proxy," + upstream
	for _, value := range []string{upstream, want} {
		var values map[string]string
		if err := json.Unmarshal(run(t, directory, append(environment, "GOPROXY="+value), "/usr/local/bin/mise", "env", "--json"), &values); err != nil {
			t.Fatal(err)
		}
		// mise env emits changes; an unchanged inherited value may be omitted.
		got := value
		if activated, changed := values["GOPROXY"]; changed {
			got = activated
		}
		if got != want {
			t.Fatalf("proxy %q activated as %q; want %q", value, got, want)
		}
	}
}
