//go:build integration

package pi_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/workflow"
)

type liveEnvironment struct {
	profile                            profile.Profile
	factory                            execution.Factory
	image                              string
	binding                            execution.BackendBinding
	stateRoot, workspaceRoot, toolRoot string
}

func TestPiLiveProviders(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_LIVE_INTEGRATION") != "1" {
		t.Skip("run mise run test-pi-live with an explicit Pi API-key auth file")
	}
	credentials, err := os.Open(os.Getenv("OMNIGREX_PI_LIVE_AUTH_FILE"))
	if err != nil {
		t.Fatal("cannot open the supplied live credential file")
	}
	defer credentials.Close()
	content, err := io.ReadAll(io.LimitReader(credentials, (1<<20)+1))
	if err != nil || len(content) > 1<<20 {
		t.Fatal("live credential file is unreadable or exceeds 1 MiB")
	}
	runProviderSmoke(t, content, true)
}

func TestPiProviderSmokeBootstrap(t *testing.T) {
	if os.Getenv("OMNIGREX_PI_LIVE_INTEGRATION") != "1" {
		t.Skip("explicit disposable provider-smoke bootstrap gate required")
	}
	// Exercise the exact golden runner, catalog, MCP, state and backend wiring
	// without contacting either real provider or requiring private credentials.
	runProviderSmoke(t, []byte(`{"openai":{"type":"api_key","key":"bootstrap-fixture-only-openai"},"opencode-go":{"type":"api_key","key":"bootstrap-fixture-only-opencode"}}`), false)
}

func runProviderSmoke(t *testing.T, content []byte, prompt bool) {
	t.Helper()
	var bundle map[string]struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(content, &bundle) != nil {
		t.Fatal("invalid live credential bundle")
	}
	secrets := []string{}
	for _, entry := range bundle {
		if entry.Key != "" {
			secrets = append(secrets, entry.Key, strings.ReplaceAll(entry.Key, "$", "$$"))
		}
	}
	root := os.Getenv("OMNIGREX_PI_LIVE_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("live smoke requires isolated absolute storage")
	}
	token := uuid.NewString()
	var calls atomic.Int32
	endpoint := liveMCPEndpoint(t, token, &calls)
	env := providerSmokeEnvironment(t, root, endpoint.URL)
	for _, selection := range []struct{ provider, model, override string }{
		{"opencode-go", "muse-spark-1.3-contributor", "OMNIGREX_PI_OPENCODE_MODEL"},
		{"openai", "o4-mini", "OMNIGREX_PI_OPENAI_MODEL"},
	} {
		t.Run(selection.provider, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			model := selection.model
			if override := os.Getenv(selection.override); override != "" {
				model = override
			}
			configuration, err := providerSmokeConfiguration(selection.provider+"/"+model, content, endpoint)
			if err != nil {
				t.Fatal("live provider selection or API-key credential is unsupported")
			}
			key := execution.Key{AssignmentID: uuid.NewString(), AgentSessionID: uuid.NewString(), AgentTurnID: uuid.NewString(), ExecutionEpoch: 1, RuntimeProfileName: profile.PiRuntimeName, RuntimeProfileVersion: profile.PiRuntimeVersion}
			ref := execution.StateRef{ID: key.AssignmentID, ParticipantID: key.AssignmentID, Backend: env.binding, Format: profile.PiStateContractVersion, RuntimeProfileSHA256: env.profile.ContentSHA256(), Image: env.profile.Binding().Image}
			states, err := dirstate.New(env.stateRoot, env.binding, qualificationStateSource{ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			if err := states.Ensure(ctx, ref); err != nil {
				t.Fatal(err)
			}
			for _, directory := range []struct{ root, name string }{{env.workspaceRoot, "workspace"}, {env.toolRoot, "mise"}, {env.toolRoot, "tool-data"}} {
				if err := os.MkdirAll(filepath.Join(directory.root, "assignment-"+key.AssignmentID, directory.name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			request, err := (pi.TurnAdapter{}).BuildProcess(env.profile, key, "", 1<<30, nil)
			if err != nil {
				t.Fatal(err)
			}
			// A local Docker image alias is fixture-only; the Profile remains exact.
			request.Image = env.image
			if env.binding.Family == "microsandbox" {
				request.State = ref
				request.State.SessionID = key.AgentSessionID
			}
			request.GoldenResources = true
			request.Requirements = []execution.Requirement{execution.ProcessIsolation, execution.ToolInstallation}
			engine, err := env.factory.New(request, conformance.NewLedger())
			if err != nil {
				t.Fatal(err)
			}
			process, err := engine.Create(ctx, request, io.Discard)
			if err != nil {
				_ = engine.Close()
				t.Fatal("cannot create isolated live execution")
			}
			cleaned := false
			cleanup := func() {
				if cleaned {
					return
				}
				cleaned = true
				stop, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				_ = process.Transport().Close()
				if err := process.Stop(stop, 5*time.Second); err != nil {
					t.Error("live execution stop failed")
					return
				}
				if err := process.Remove(stop); err != nil {
					t.Error("live execution removal failed")
				}
				if err := engine.Close(); err != nil {
					t.Error("live execution close failed")
				}
			}
			t.Cleanup(cleanup)
			if err := process.Start(ctx); err != nil {
				t.Fatal("cannot start isolated live execution")
			}
			configuration.Session = &agent.SessionIdentity{AgentSessionID: key.AgentSessionID, RuntimeSessionID: key.AgentSessionID}
			connection, err := pi.Start(ctx, process.Transport(), configuration, pi.Options{RequestTimeout: 30 * time.Second})
			if err != nil {
				t.Fatal("production Pi bootstrap failed")
			}
			t.Cleanup(func() { _ = connection.Close() })
			ready, err := connection.Prepare(ctx)
			if err != nil || ready.RuntimeSessionID != key.AgentSessionID {
				t.Fatal("exact model, thinking, authenticated MCP, or Session preparation failed")
			}
			if prompt {
				outcome, err := connection.Prompt(ctx, "Reply with exactly OK. Do not use tools.", nil)
				if err != nil || !outcome.Admitted || outcome.Status != agent.Completed {
					t.Fatal("live provider did not complete an admitted production Pi prompt")
				}
			}
			history := &historySink{}
			if _, err := connection.Replay(ctx, agent.HistoryCursor{}, history); err != nil || len(history.events) == 0 {
				t.Fatal("live Session history is unavailable")
			}
			if calls.Load() != 0 {
				t.Fatal("live smoke unexpectedly invoked an MCP business tool")
			}
			_ = connection.Close()
			cleanup()
			if t.Failed() {
				return
			}
			var archive bytes.Buffer
			if err := states.Export(ctx, ref, &archive); err != nil {
				t.Fatal("cannot audit stopped live Session storage")
			}
			if err := checkProviderSmokeArchive(archive.Bytes(), key.AgentSessionID, append(secrets, token)); err != nil {
				t.Fatal(err)
			}
			if prompt {
				t.Logf("PASS: live %s/%s through the production runner, exact configuration, authenticated MCP, durable Session and credential-free retained state", selection.provider, model)
			} else {
				t.Logf("PASS: %s/%s production bootstrap and state audit without a provider request", selection.provider, model)
			}
		})
	}
}

func providerSmokeConfiguration(model string, credentials []byte, endpoint agent.MCPEndpoint) (pi.Configuration, error) {
	p, err := agentprofile.Parse(".omnigrex/team/provider-smoke.md", []byte(fmt.Sprintf("---\nname: provider-smoke\nrole: REVIEWER\nruntime: pi-rpc/v2\nmodel: %q\nthinking: low\npermissions:\n  read: deny\n---\nVerify provider connectivity without using tools.\n", model)), role.BuiltinPolicyCatalog())
	if err != nil {
		return pi.Configuration{}, pi.ErrConfiguration
	}
	policy, _ := role.BuiltinPolicyCatalog().Lookup(role.Reviewer)
	policy.MCPTools = []string{"get_issue"}
	return pi.Render(p, policy, agentinstructions.Operator{}, workflow.StageDefinition{Role: role.Reviewer, Instructions: "Reply briefly without calling tools."}, workflow.TurnPurpose("provider_smoke"), endpoint, credentials)
}

func liveMCPEndpoint(t *testing.T, token string, calls *atomic.Int32) agent.MCPEndpoint {
	t.Helper()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: providerSmokeMCP(token, calls), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() {
			return agent.MCPEndpoint{Name: "omnigrex", URL: "http://" + net.JoinHostPort(ip.String(), fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)) + "/mcp", Headers: []agent.Header{{Name: "Authorization", Value: "Bearer " + token}}}
		}
	}
	t.Fatal("live smoke wrapper has no reachable IPv4 address")
	return agent.MCPEndpoint{}
}

func providerSmokeMCP(token string, calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&message) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(message.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		response := map[string]any{"jsonrpc": "2.0", "id": message.ID}
		switch message.Method {
		case "initialize":
			response["result"] = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "provider-smoke", "version": "1"}}
		case "tools/list":
			response["result"] = map[string]any{"tools": []any{map[string]any{"name": "get_issue", "description": "Unused live-smoke fixture", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		default:
			calls.Add(1)
			response["error"] = map[string]any{"code": -32601, "message": "Business tools are unavailable in provider smoke"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
}

func checkProviderSmokeArchive(archive []byte, sessionID string, secrets []string) error {
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(archive, []byte(secret)) {
			return errors.New("provider or MCP credential found in retained live Session storage")
		}
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("live Session archive contains no exact nonempty Session file")
		}
		if err != nil {
			return errors.New("live Session archive is invalid")
		}
		if header.Name == "sessions/"+sessionID+"/session.jsonl" && header.Typeflag == tar.TypeReg && header.Size > 0 {
			return nil
		}
	}
}

func TestProviderSmokeConfigurationAndArchiveAudit(t *testing.T) {
	endpoint := agent.MCPEndpoint{Name: "omnigrex", URL: "http://127.0.0.1:8844/mcp", Headers: []agent.Header{{Name: "Authorization", Value: "Bearer mcp-fixture-key"}}}
	for _, credential := range []string{`{"openai":{"type":"api_key","key":"literal-$9-key"},"unrelated":{"type":"api_key","key":"excluded-provider-key"}}`, `{"openai":{"type":"oauth","access":"unsupported"}}`, `{}`} {
		configuration, err := providerSmokeConfiguration("openai/o4-mini", []byte(credential), endpoint)
		if strings.Contains(credential, `"api_key"`) {
			if err != nil || configuration.Selection.Model != "openai/o4-mini" || configuration.Selection.Thinking != "low" || len(configuration.Tools) != 0 || configuration.RepositorySkills {
				t.Fatal("smoke lost exact model/thinking or gained local tools")
			}
			if bytes.Contains(configuration.Auth, []byte("excluded-provider-key")) || !bytes.Contains(configuration.Auth, []byte("literal-$$9-key")) {
				t.Fatal("smoke bypassed production credential filtering/encoding")
			}
		} else if !errors.Is(err, pi.ErrConfiguration) {
			t.Fatal("smoke accepted unsupported credentials")
		}
	}
	for _, payload := range []string{"retained conversation", "provider-fixture-key"} {
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		if err := writer.WriteHeader(&tar.Header{Name: "sessions/exact/session.jsonl", Mode: 0600, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := checkProviderSmokeArchive(archive.Bytes(), "exact", []string{"provider-fixture-key"}); (err != nil) != strings.Contains(payload, "provider-fixture-key") {
			t.Fatal("credential audit did not inspect retained bytes")
		}
		if err := checkProviderSmokeArchive(archive.Bytes(), "foreign", nil); err == nil {
			t.Fatal("audit accepted missing exact Session")
		}
	}
}

func TestProviderSmokeMCPRequiresAuthenticationAndRejectsBusinessCalls(t *testing.T) {
	var calls atomic.Int32
	handler := providerSmokeMCP("fixture-token", &calls)
	for _, tc := range []struct {
		method, token string
		status        int
	}{{"initialize", "wrong", 401}, {"initialize", "fixture-token", 200}, {"tools/list", "fixture-token", 200}, {"tools/call", "fixture-token", 200}} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, tc.method)))
		request.Header.Set("Authorization", "Bearer "+tc.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("MCP %s status = %d", tc.method, response.Code)
		}
		if tc.method == "tools/call" && !strings.Contains(response.Body.String(), `"error"`) {
			t.Fatal("fixture granted business authority")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("fixture did not track forbidden business activity")
	}
}

// Docker fixtures use this concrete factory; native builds import the same
// golden image into the SDK and retain the same production Pi launch path.
func dockerProviderSmokeEnvironment(t *testing.T) liveEnvironment {
	t.Helper()
	if os.Getenv("OMNIGREX_PI_BACKEND") != "docker" {
		t.Fatal("Docker smoke requires the Docker backend")
	}
	p, err := profile.NewPiV2(os.Getenv("OMNIGREX_PI_LIVE_IMAGE"), profile.Platform{OS: "linux", Arch: os.Getenv("OMNIGREX_PI_LIVE_ARCH")})
	if err != nil {
		t.Fatal(err)
	}
	volume := os.Getenv("OMNIGREX_PI_LIVE_VOLUME")
	root := os.Getenv("OMNIGREX_PI_LIVE_ROOT")
	return liveEnvironment{profile: p, image: os.Getenv("OMNIGREX_PI_LIVE_LOCAL_IMAGE"), stateRoot: root, workspaceRoot: root, toolRoot: root, binding: execution.BackendBinding{Family: "docker", Version: 1, Namespace: volume}, factory: docker.ExecutionFactory{Network: os.Getenv("OMNIGREX_PI_LIVE_NETWORK"), RuntimeStateVolume: volume, WorkspaceVolume: volume, MiseVolume: volume}}
}
