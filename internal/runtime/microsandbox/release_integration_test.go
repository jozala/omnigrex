//go:build integration && microsandbox && cgo && linux

package microsandbox

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

// Assert what is actually loaded/packaged, not only that go.mod and msb --version
// look plausible. A stale FFI or firmware can otherwise survive a CLI-only bump.
func TestNativeCoordinatedReleasePayload(t *testing.T) {
	backend, _, _ := nativeFixture(t)
	resolved, err := sdk.ResolveRuntime(sdk.RuntimeConfig{})
	if err != nil || resolved.MSBPath != backend.options.Executable || resolved.LibkrunfwPath != backend.options.Firmware {
		t.Fatalf("explicit deployment runtime overrides lost: %+v, %v", resolved, err)
	}
	if info, err := sdk.DefaultBackendInfo(); err != nil || info.Kind != sdk.BackendLocal {
		t.Fatalf("backend: %+v, %v", info, err)
	}
	assertReleaseSHA256(t, backend.options.Executable, "02230b0d0ccab4da487c96282ef06bc366a603f0b95201e999cf9e13df31767e")
	assertReleaseSHA256(t, backend.options.Firmware, "3854efcd3fe625528fa00207d97586d9790db8c436bb6881a43986604336d18e")
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	libraries := map[string]bool{}
	for _, line := range strings.Split(string(maps), "\n") {
		if !strings.Contains(line, "libmicrosandbox_go_ffi") {
			continue
		}
		fields := strings.Fields(line)
		path := fields[len(fields)-1]
		if !strings.HasPrefix(path, backend.options.Home+"/lib/v0.7.11/") {
			t.Fatalf("unexpected loaded SDK FFI: %s", path)
		}
		libraries[path] = true
	}
	if len(libraries) != 1 {
		t.Fatalf("loaded FFI libraries: %d, want exactly one", len(libraries))
	}
	for path := range libraries {
		assertReleaseSHA256(t, path, "4189cd64d83d138e61dd11d8891f8bb4614da688fec50fe0ab5ef64012b2f3fe")
	}
	agentd := os.Getenv("MSB_AGENTD_PATH")
	if agentd != "/usr/local/lib/omnigrex/agentd" {
		t.Fatalf("patched external agentd override: %q", agentd)
	}
	data, err := os.ReadFile("/usr/share/omnigrex/microsandbox-agentd-source")
	if err != nil || strings.TrimSpace(string(data)) != "df3e0f7dd69280b88f6e28d6d3cf5fe4bb4a87a0 patched-domain-cgroups" {
		t.Fatalf("patched source provenance: %q, %v", data, err)
	}
	if stat, err := os.Stat(agentd); err != nil || stat.Mode()&0111 == 0 {
		t.Fatal("patched agentd payload is not executable")
	}
	t.Log("COORDINATED RELEASE PASS: sdk=0.7.11 cli_sha256=pass firmware_sha256=pass loaded_ffi_sha256=pass backend=local explicit_paths=preserved agentd=patched-v0.7.11")
}

func assertReleaseSHA256(t *testing.T, path, expected string) {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", digest.Sum(nil)); got != expected {
		t.Fatalf("release payload checksum for %s: got %s want %s", path, got, expected)
	}
}
