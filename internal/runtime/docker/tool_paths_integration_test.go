//go:build integration

package docker_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestToolPathMountIsDiskBackedAndAssignmentIsolated(t *testing.T) {
	profile, err := runtimeprofile.NewOpenCodeV1(
		"registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		runtimeprofile.Platform{OS: "linux", Arch: "amd64"},
	)
	if err != nil {
		t.Fatal(err)
	}
	contract := profile.Contract()
	const toolMount = "tool-data"
	var target string
	for _, mount := range contract.Mounts {
		if mount.Name == toolMount {
			target = mount.Path
		}
	}
	if target != workspace.TurnPathMount || target == contract.Workspace || strings.HasPrefix(target, contract.Workspace+"/") {
		t.Fatalf("tool data mount %q must be the Runtime Profile's path outside workspace %q", target, contract.Workspace)
	}
	image := environmentOrDefault("OMNIGREX_OPENCODE_IMAGE", "omnigrex/opencode:1.18.29")
	volume := fmt.Sprintf("omnigrex-tool-paths-%d", time.Now().UnixNano())
	dockerCommand(t, "volume", "create", volume)
	t.Cleanup(func() { dockerCleanup(t, "volume", "rm", "--force", volume) })
	rendered, err := opencode.Render(opencode.RoleDeveloper, opencode.Profile{
		Instructions: "Keep tool caches outside the workspace.", Model: "fake/fake-model", Steps: 1,
		Permissions: opencode.PermissionPolicy{"bash": opencode.PermissionAllow},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, spec, err := opencode.BuildProcess(profile, rendered, opencode.ProviderCredentials{Role: opencode.RoleDeveloper, Content: []byte(`{}`)}, opencode.ProcessOptions{
		Name: "omnigrex-tool-path-test", Network: "none", AssignmentID: "a", AgentSessionID: "session", AgentTurnID: "turn-a", ExecutionEpoch: 1, MemoryBytes: 512 << 20,
		VolumeBindings:     map[string]string{"mise": volume, "state": "state-volume", "tool-data": volume, "workspace": "workspace-volume"},
		AssignmentSubpaths: map[string]string{"mise": "assignment-a/mise", "state": "assignment-a/runtime-state", "tool-data": "assignment-a/tool-data", "workspace": "assignment-a/workspace"},
		Environment:        map[string]string{"TMPDIR": target + "/turn/turn-a/build"},
	})
	if err != nil {
		t.Fatalf("compile Runtime Process tool-data mount: %v", err)
	}
	var toolData dockerruntime.VolumeMount
	for _, mount := range spec.Volumes {
		if mount.Target == target {
			toolData = mount
		}
	}
	if toolData.Name != volume || toolData.Subpath != "assignment-a/tool-data" || toolData.Target != target {
		t.Fatalf("compiled tool-data mount = %#v", toolData)
	}
	if !slices.Contains(spec.Environment, "TMPDIR="+target+"/turn/turn-a/build") {
		t.Fatalf("compiled TMPDIR did not use tool-data mount: %q", spec.Environment)
	}
	dockerCommand(t, "run", "--rm", "--user", "0:0", "--network", "none",
		"--mount", "type=volume,src="+volume+",dst=/data", "--entrypoint", "sh", image,
		"-ec", "mkdir -p /data/assignment-a/mise /data/assignment-a/tool-data/turn/turn-a/build /data/assignment-b/tool-data && chown -R 10001:10001 /data/assignment-a /data/assignment-b")
	dockerCommand(t, "run", "--rm", "--user", "10001:10001", "--read-only", "--network", "none",
		"--cap-drop", "ALL", "--tmpfs", "/tmp/opencode:rw,size=64m,uid=10001,gid=10001",
		"--mount", "type=volume,src="+toolData.Name+",dst="+toolData.Target+",volume-subpath="+toolData.Subpath,
		"--env", "TMPDIR="+target+"/turn/turn-a/build", "--entrypoint", "sh", image,
		"-ec", "dd if=/dev/zero of=\"$TMPDIR/work\" bs=1M count=65 2>/dev/null && test -s \"$TMPDIR/work\"")
	dockerCommand(t, "run", "--rm", "--user", "10001:10001", "--read-only", "--network", "none",
		"--mount", "type=volume,src="+volume+",dst="+target+",volume-subpath=assignment-b/tool-data",
		"--entrypoint", "sh", image, "-ec", "test ! -e "+target+"/turn/turn-a/build/work")
}
