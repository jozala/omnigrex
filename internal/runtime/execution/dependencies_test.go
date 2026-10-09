package execution_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestCoreDependencyGraphDoesNotReachExecutionImplementations(t *testing.T) {
	for _, tags := range []string{"", "integration", "microsandbox", "integration,microsandbox"} {
		command := exec.Command("go", "list", "-deps", "-json", "-tags="+tags,
			"github.com/jozala/omnigrex/internal/agentturn", "github.com/jozala/omnigrex/internal/startup",
			"github.com/jozala/omnigrex/internal/workflow", "github.com/jozala/omnigrex/internal/workflowaction",
			"github.com/jozala/omnigrex/internal/store", "github.com/jozala/omnigrex/internal/runtime/opencode",
			"github.com/jozala/omnigrex/internal/runtime/execution", "github.com/jozala/omnigrex/internal/runtime/agent")
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
			for _, forbidden := range []string{"github.com/moby/", "github.com/docker/", "github.com/superradcompany/microsandbox/", "github.com/jozala/omnigrex/internal/runtime/docker", "github.com/jozala/omnigrex/internal/runtime/microsandbox", "github.com/jozala/omnigrex/internal/runtime/pi"} {
				if strings.HasPrefix(pkg.ImportPath, forbidden) {
					t.Errorf("core graph reaches %s (tags %q)", pkg.ImportPath, tags)
				}
			}
		}
	}
}
