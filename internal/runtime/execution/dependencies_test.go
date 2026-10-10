package execution_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCoreDependencyGraphDoesNotReachExecutionImplementations(t *testing.T) {
	for _, target := range []struct{ os, arch string }{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		for _, tags := range []string{"", "integration", "microsandbox", "integration,microsandbox"} {
			command := exec.Command("go", "list", "-deps", "-json", "-tags="+tags,
				"github.com/jozala/omnigrex/internal/agentturn", "github.com/jozala/omnigrex/internal/startup",
				"github.com/jozala/omnigrex/internal/workflow", "github.com/jozala/omnigrex/internal/workflowaction",
				"github.com/jozala/omnigrex/internal/store", "github.com/jozala/omnigrex/internal/runtime/opencode",
				"github.com/jozala/omnigrex/internal/runtime/execution", "github.com/jozala/omnigrex/internal/runtime/agent",
				"github.com/jozala/omnigrex/internal/runtime/session", "github.com/jozala/omnigrex/internal/mcp",
				"github.com/jozala/omnigrex/internal/retention", "github.com/jozala/omnigrex/internal/workspace")
			command.Env = append(os.Environ(), "GOOS="+target.os, "GOARCH="+target.arch, "CGO_ENABLED=0")
			output, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(strings.NewReader(string(output)))
			for {
				var pkg struct{ ImportPath string }
				if err := decoder.Decode(&pkg); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if backendPackage(pkg.ImportPath) || packageFamily(pkg.ImportPath, "github.com/jozala/omnigrex/internal/runtime/pi") {
					t.Errorf("core graph reaches %s (%s/%s, tags %q)", pkg.ImportPath, target.os, target.arch, tags)
				}
			}
		}
	}
}

// Parse every production file, including native CGO and platform/tag variants
// that the local Go toolchain cannot compile. Concrete constructors belong only
// to the composition root or the implementation that owns them.
func TestProductionSourceKeepsExecutionAndAgentBoundaries(t *testing.T) {
	root := filepath.Clean("../..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		owner := filepath.ToSlash(filepath.Dir(relative))
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			dependency, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if boundaryImportForbidden(owner, dependency) {
				t.Errorf("%s imports implementation-owned %s", relative, dependency)
			}
		}
		if owner == "runtime/execution" || owner == "runtime/agent" {
			ast.Inspect(file, func(node ast.Node) bool {
				field, ok := node.(*ast.Field)
				if !ok {
					return true
				}
				for _, name := range field.Names {
					if !name.IsExported() {
						continue
					}
					switch name.Name {
					case "HostPath", "ContainerID", "VolumeName", "Labels", "VendorOptions":
						t.Errorf("%s exposes implementation-specific field %s", relative, name.Name)
					}
					if m, ok := field.Type.(*ast.MapType); ok {
						if value, ok := m.Value.(*ast.Ident); ok && value.Name == "any" {
							t.Errorf("%s exposes untyped option map %s", relative, name.Name)
						}
						if value, ok := m.Value.(*ast.InterfaceType); ok && len(value.Methods.List) == 0 {
							t.Errorf("%s exposes untyped option map %s", relative, name.Name)
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func boundaryImportForbidden(owner, dependency string) bool {
	const internal = "github.com/jozala/omnigrex/internal/"
	composition := packageFamily(owner, "runtime/composition")
	concrete := packageFamily(owner, "runtime/docker") || packageFamily(owner, "runtime/microsandbox")
	if backendPackage(dependency) && !composition && !concrete {
		return true
	}
	if packageFamily(dependency, internal+"runtime/pi") && !composition && !packageFamily(owner, "runtime/pi") {
		return true
	}
	if owner == "runtime/execution" || owner == "runtime/agent" {
		if strings.HasPrefix(dependency, internal) && dependency != internal+"runtime/agentevent" && dependency != internal+"uuidtext" {
			return true
		}
	}
	// Transitional ACP vocabulary is confined to the existing compatibility
	// consumers until Step 17. Neutral contracts and Workflow policy cannot use it.
	if dependency == internal+"runtime/acp" && (owner == "workflow" || owner == "workflowaction" || owner == "mcp" || owner == "startup" || owner == "store") {
		return true
	}
	return false
}

func packageFamily(path, family string) bool {
	return path == family || strings.HasPrefix(path, family+"/")
}

func backendPackage(path string) bool {
	for _, family := range []string{"github.com/moby", "github.com/docker", "github.com/superradcompany/microsandbox", "github.com/jozala/omnigrex/internal/runtime/docker", "github.com/jozala/omnigrex/internal/runtime/microsandbox"} {
		if packageFamily(path, family) {
			return true
		}
	}
	return false
}

func TestBoundaryRulesRejectLeakageAndPermitOwnedComposition(t *testing.T) {
	for _, tc := range []struct {
		owner, dependency string
		forbidden         bool
	}{
		{"agentturn", "github.com/moby/moby/api/types/container", true},
		{"runtime/pi", "github.com/superradcompany/microsandbox/sdk/go", true},
		{"runtime/execution", "github.com/jozala/omnigrex/internal/store", true},
		{"runtime/agent", "github.com/jozala/omnigrex/internal/runtime/acp", true},
		{"workflow", "github.com/jozala/omnigrex/internal/runtime/pi", true},
		{"runtime/composition", "github.com/jozala/omnigrex/internal/runtime/pi", false},
		{"runtime/docker", "github.com/moby/moby/client", false},
		{"runtime/execution", "github.com/jozala/omnigrex/internal/uuidtext", false},
		{"mcp", "github.com/jozala/omnigrex/internal/runtime/docker/helpers", true},
		{"mcp", "github.com/jozala/omnigrex/internal/runtime/pi/protocol", true},
		{"runtime/composition", "github.com/jozala/omnigrex/internal/runtime/microsandbox/native", false},
		{"runtime/pi", "github.com/jozala/omnigrex/internal/runtime/pi/protocol", false},
		{"runtime/agent", "github.com/jozala/omnigrex/internal/runtime/dockerish", true},
		{"workspace", "github.com/jozala/omnigrex/internal/runtime/dockerish", false},
	} {
		if got := boundaryImportForbidden(tc.owner, tc.dependency); got != tc.forbidden {
			t.Errorf("%s -> %s: forbidden=%t", tc.owner, tc.dependency, got)
		}
	}
}
