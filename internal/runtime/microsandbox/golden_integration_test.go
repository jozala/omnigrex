//go:build integration && microsandbox && cgo

package microsandbox

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
)

func TestMicrosandboxGoldenEnvironmentWorkingRevisionAndSessionContinuation(t *testing.T) {
	backend, base, ctx := nativeFixture(t)
	const archive = "/tmp/golden-image.tar"
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("golden fixture archive missing; run mise run test-microsandbox-integration")
	}
	images, err := sdk.Image.Load(ctx, archive, "registry.example/omnigrex/opencode:fixture")
	if err != nil || len(images) == 0 {
		t.Fatalf("import golden fixture: %v", err)
	}
	image := "registry.example/omnigrex/opencode@" + images[0].ManifestDigest()
	if _, err := sdk.Image.Load(ctx, archive, image); err != nil {
		t.Fatalf("bind exact imported fixture: %v", err)
	}
	runtime, err := profile.NewOpenCodeV1(image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	for index, role := range []opencode.Role{opencode.RoleDeveloper, opencode.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			key := base.Key
			key.AssignmentID = strings.Replace(base.Key.AssignmentID, "000000000001", "00000000000"+string(rune('2'+index)), 1)
			key.AgentSessionID = strings.Replace(base.Key.AgentSessionID, "000000000001", "00000000000"+string(rune('2'+index)), 1)
			key.RuntimeProfileName, key.RuntimeProfileVersion = "opencode-acp", "v1"
			workspace := backend.location(key, "workspace")
			if err := os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			ref := execution.StateRef{ID: key.AssignmentID, ParticipantID: key.AssignmentID, SessionID: key.AgentSessionID, Backend: backend.Binding(), Format: "opencode-data/v1", RuntimeProfileSHA256: runtime.ContentSHA256(), Image: image}
			source := &stateFixtureSource{ref: ref}
			states, err := dirstate.New(backend.options.StateRoot, backend.Binding(), source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(backend.options.StateRoot, 0700); err != nil {
				t.Fatal(err)
			}
			if err := states.Ensure(ctx, ref); err != nil {
				t.Fatal(err)
			}
			rendered, err := opencode.Render(role, opencode.Profile{Instructions: "Use authorized Role capabilities.", Model: "openai/gpt-5", Steps: 1, Permissions: opencode.PermissionPolicy{"read": opencode.PermissionAllow}})
			if err != nil {
				t.Fatal(err)
			}
			var sessionID string
			for turn := 0; turn < 2; turn++ {
				key.ExecutionEpoch = int64(1 + index*2 + turn)
				if err := os.WriteFile(filepath.Join(workspace, "mise.toml"), []byte("[hooks]\npostinstall = \"printf working-revision-"+string(rune('1'+turn))+" > installed-revision\"\n[env]\nWORKING_REVISION = '"+string(rune('1'+turn))+"'\n"), 0600); err != nil {
					t.Fatal(err)
				}
				request, err := opencode.BuildProcess(runtime, rendered, opencode.ProviderCredentials{Role: role, Content: []byte(`{}`)}, opencode.ProcessOptions{Key: key, MemoryBytes: 1 << 30})
				if err != nil {
					t.Fatal(err)
				}
				request.Command = []string{environment.Executable, "run", "acp"}
				request.Environment = environment.BindEnvironment(request.Environment)
				request.GoldenResources = true
				request.Requirements = []execution.Requirement{execution.ProcessIsolation, execution.ToolInstallation}
				request.Workspace = execution.WorkspaceRef{Key: key, Seed: strings.Repeat("a", 40)}
				request.State = ref
				process := launchFixture(t, ctx, backend, request)
				client := acp.NewClient(process.Transport(), acp.ClientOptions{RequiredCapabilities: acp.RequiredCapabilities{SessionList: true, SessionResume: true, SessionLoad: true}})
				if _, err := client.Initialize(ctx); err != nil {
					t.Fatal(err)
				}
				if turn == 0 {
					session, err := client.CreateSession(ctx, acp.CreateSessionRequest{CWD: "/workspace"})
					if err != nil {
						t.Fatal(err)
					}
					sessionID = session.ID
				} else {
					if err := client.ContinueSession(ctx, acp.ContinueSessionRequest{SessionID: sessionID, CWD: "/workspace"}); err != nil {
						t.Fatal(err)
					}
				}
				content, err := os.ReadFile(filepath.Join(workspace, "installed-revision"))
				if err != nil || string(content) != "working-revision-"+string(rune('1'+turn)) {
					t.Fatalf("working revision tooling=%q,%v", content, err)
				}
				_ = client.Close()
				if err := process.Stop(ctx, 10*time.Second); err != nil {
					t.Fatal(err)
				}
				if err := process.Remove(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	// A diagnostic uses no workspace/state attachments, initializes the pinned
	// runtime, and leaves no diagnostic VM after cleanup.
	process, err := backend.StartDiagnostic(ctx, runtime, image, 512<<20, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	client := acp.NewClient(process.Transport(), acp.ClientOptions{})
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := process.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	resources, _, err := backend.platform.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if resource.Labels[diagnosticLabel] == "true" {
			t.Fatal("diagnostic left resources")
		}
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	host := ""
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			host = ip.String()
			break
		}
	}
	if host == "" {
		t.Fatal("no doctor MCP route")
	}
	command := exec.CommandContext(ctx, "/usr/local/bin/omnigrex", "doctor", "--repository", "fixture/repository")
	command.Env = append(os.Environ(), "OMNIGREX_OPENCODE_ACP_V1_IMAGE="+image, "OMNIGREX_OPENCODE_ACP_V1_PLATFORM=linux/amd64", "OMNIGREX_GITHUB_DEVELOPER_APP_ID=1", "OMNIGREX_GITHUB_REVIEWER_APP_ID=2", "OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE=/dev/null", "OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE=/dev/null", "OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE=/dev/null", "OMNIGREX_PROVIDER_CREDENTIALS_FILE=/dev/null", "OMNIGREX_DATABASE_URL=postgres://fixture@127.0.0.1:1/fixture?sslmode=disable", "OMNIGREX_MICROSANDBOX_HOME="+backend.options.Home, "OMNIGREX_MICROSANDBOX_NAMESPACE="+backend.options.Namespace, "OMNIGREX_WORKSPACE_ROOT="+backend.options.WorkspaceRoot, "OMNIGREX_RUNTIME_STATE_ROOT="+backend.options.StateRoot, "OMNIGREX_MISE_ROOT="+backend.options.ToolRoot, "OMNIGREX_READINESS_TIMEOUT=20s", "OMNIGREX_MCP_ENDPOINT_URL=http://"+host+":1/mcp")
	output, _ := command.CombinedOutput()
	// Independent secret/GitHub/PostgreSQL checks deliberately fail on this
	// fixture, while the real selected-backend and authenticated MCP probes run.
	if !strings.Contains(string(output), "PASS microsandbox-kvm-resources") || !strings.Contains(string(output), "PASS acp-runtime") {
		t.Fatalf("selected-backend doctor failed:\n%s", output)
	}
	t.Log("PASS: imported digest-pinned golden image, both Roles' working-revision setup, fresh-VM OpenCode Session Continuation, and disposable diagnostic initialization")
}
