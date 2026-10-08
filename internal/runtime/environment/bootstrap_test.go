package environment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBootstrapUsesCheckoutToolsWithoutAuthorityAndPreservesControlEnvironment(t *testing.T) {
	root := t.TempDir()
	checkout, data := filepath.Join(root, "checkout"), filepath.Join(root, "data")
	if err := os.Mkdir(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "mise.toml"), []byte("working-revision-tools"), 0o644); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	script := `#!/bin/sh
set -eu
test -z "${OPENCODE_AUTH_CONTENT:-}"
test -z "${OMNIGREX_TOKEN:-}"
test -z "${NODE_OPTIONS:-}"
test "$(cat mise.toml)" = working-revision-tools
if [ "$1" = install ]; then
  printf installed > "$MISE_DATA_DIR/installed"
  mkdir "$TMPDIR/restrictive"
  printf scratch > "$TMPDIR/restrictive/file"
  chmod 000 "$TMPDIR/restrictive"
else
  printf '{"PATH":"/opt/development/bin:/usr/bin:/bin","PROJECT_MODE":"working-revision"}'
fi
`
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	inherited := BindEnvironment([]string{"OPENCODE_AUTH_CONTENT=provider-secret", "OMNIGREX_TOKEN=authority", "NODE_OPTIONS=protected", "HOME=/home/opencode", "TMPDIR=/disk/scratch"})
	inherited = append(inherited, "OPENCODE_CONFIG_DIR=/workspace/poison", "MISE_CONFIG_FILE=/workspace/poison", "PI_CODING_AGENT_DIR=/workspace/poison")
	values, err := Bootstrap(context.Background(), checkout, data, mise, inherited, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PROJECT_MODE=working-revision", "PATH=/opt/development/bin:/usr/bin:/bin", "OPENCODE_AUTH_CONTENT=provider-secret", "HOME=/home/opencode", "TMPDIR=/disk/scratch"} {
		if !slices.Contains(values, want) {
			t.Fatalf("missing %s in runtime activation", want)
		}
	}
	if slices.Contains(values, "NODE_OPTIONS=protected") {
		t.Fatal("image loader settings reached the Runtime Process")
	}
	for _, entry := range values {
		if strings.Contains(entry, "poison") {
			t.Fatal("unrequested image control environment reached the agent")
		}
	}
	if _, err := os.Stat(filepath.Join(data, "installed")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "tmp")); !os.IsNotExist(err) {
		t.Fatal("setup scratch retained")
	}
}

func TestBootstrapRejectsRuntimeInjectionAndBoundsOutput(t *testing.T) {
	for _, value := range []string{`{"NODE_OPTIONS":"--require=/workspace/poison.js"}`, `{"PI_CODING_AGENT_DIR":"/workspace"}`, `{"OPENCODE_DISABLE_PROJECT_CONFIG":"false"}`, `{"LD_PRELOAD":"/workspace/poison.so"}`, `{"PATH":".:/bin"}`, strings.Repeat("x", 121<<10)} {
		t.Run(value[:min(len(value), 35)], func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "mise.toml"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(root, "activation")
			if err := os.WriteFile(fixture, []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
			mise := filepath.Join(root, "mise")
			if err := os.WriteFile(mise, []byte("#!/bin/sh\nif [ \"$1\" = env ]; then cat "+fixture+"; fi\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := Bootstrap(context.Background(), root, filepath.Join(root, "data"), mise, BindEnvironment([]string{"HOME=/home/opencode"}), nil); err == nil {
				t.Fatal("accepted unsafe activation")
			}
		})
	}
}

func TestBootstrapPreservesAdmittedCachePathsAfterMiseActivation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mise.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	script := `#!/bin/sh
if [ "$1" = env ]; then
  printf '{"GOCACHE":"/workspace/cache","GOPATH":"/tmp/gopath","CUSTOM_CACHE":"/workspace/custom","PROJECT_MODE":"working-revision"}'
fi
`
	if err := os.WriteFile(mise, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	admitted := []string{
		"HOME=/home/opencode", "GOCACHE=/participant/tool-data/go-cache",
		"GOPATH=/participant/tool-data/go-path", "CUSTOM_CACHE=/participant/tool-data/custom",
	}
	values, err := Bootstrap(context.Background(), root, filepath.Join(root, "data"), mise, BindEnvironment(admitted), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range append(admitted, "PROJECT_MODE=working-revision") {
		if !slices.Contains(values, want) {
			t.Errorf("bootstrap environment lost admitted or repository value %q: %v", want, values)
		}
	}
}

func TestBootstrapCancellationAndFailureDoNotLeakRepositoryOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mise.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mise := filepath.Join(root, "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nprintf sensitive >&2\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Bootstrap(ctx, root, filepath.Join(root, "data"), mise, BindEnvironment([]string{"HOME=/home/opencode"}), nil)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe failure: %v", err)
	}
}

func TestToolReplacementPreservesSiblingStateCachesAndSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "mise")
	for _, name := range []string{"mise/stale", "runtime-state", "tool-data", "workspace", "external"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "marker"), []byte(name), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "external"), filepath.Join(data, "external-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(data, "stale"), 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(context.Background(), filepath.Join(root, "workspace"), data, "/does-not-exist", BindEnvironment([]string{"HOME=/home/opencode"}), nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime-state", "tool-data", "workspace", "external"} {
		if content, err := os.ReadFile(filepath.Join(root, name, "marker")); err != nil || string(content) != name {
			t.Fatalf("replacement changed %s: %s %v", name, content, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(data, "stale")); !os.IsNotExist(err) {
		t.Fatal("stale tool installation retained")
	}
}
