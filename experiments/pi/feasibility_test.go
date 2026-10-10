//go:build integration

package pi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const nodeImage = "docker.io/library/node@sha256:b506e7321f176aae77317f99d67a24b272c1f09f1d10f1761f2773447d8da26c"

type probe struct {
	t                *testing.T
	ctx              context.Context
	image            string
	network          string
	server           string
	serverURL        string
	backend          string
	wrapper          string
	state            string
	secrets          []string
	captureWorkspace bool
	workspaceVolume  string
}

func newProbe(t *testing.T) *probe {
	t.Helper()
	if os.Getenv("OMNIGREX_PI_FEASIBILITY") != "1" {
		t.Skip("set OMNIGREX_PI_FEASIBILITY=1; see experiments/pi/README.md")
	}
	backend := os.Getenv("OMNIGREX_PI_BACKEND")
	if backend == "" {
		backend = "docker"
	}
	if backend != "docker" && backend != "microsandbox" {
		t.Fatal("OMNIGREX_PI_BACKEND must be docker or microsandbox")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	prefix := "omnigrex-pi-probe-" + strings.ToLower(rand.Text())
	p := &probe{t: t, ctx: ctx, image: prefix + ":test", network: prefix, server: prefix + "-server", backend: backend, state: prefix + "-state"}
	p.command(10*time.Minute, nil, "build", "--file", "Dockerfile", "--tag", p.image, "../..")
	t.Cleanup(func() { p.cleanup("image", "rm", p.image) })
	version := p.command(time.Minute, nil, "run", "--rm", "--network", "none", "--entrypoint", "node", p.image, "--version")
	if version != "v24.14.1" {
		t.Fatalf("unexpected Node version: %s", version)
	}
	version = p.command(time.Minute, nil, "run", "--rm", "--network", "none", "--entrypoint", "/opt/probe/node_modules/.bin/pi", p.image, "--version")
	if version != "1.0.4" {
		t.Fatalf("unexpected Pi version: %s", version)
	}
	t.Logf("Pi %s / Node v24.14.1 on %s", version, backend)
	p.command(time.Minute, nil, "network", "create", p.network)
	t.Cleanup(func() { p.cleanup("network", "rm", p.network) })
	t.Cleanup(func() { p.cleanup("rm", "-f", "-v", p.server) })
	p.command(time.Minute, nil, "run", "-d", "--rm", "--name", p.server, "--network", p.network,
		"--label", "io.omnigrex.experiment=pi-feasibility",
		"--memory", "256m", "--cpus", "1", "--pids-limit", "128", "--entrypoint", "node", p.image, "/opt/probe/fixtures/server.mjs")
	ip := p.command(time.Minute, nil, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", p.server)
	p.serverURL = "http://" + ip + ":8080"
	p.control("/health", nil)
	if backend == "docker" {
		p.command(time.Minute, nil, "volume", "create", p.state)
		t.Cleanup(func() { p.cleanup("volume", "rm", p.state) })
	} else {
		gid := os.Getenv("OMNIGREX_MICROSANDBOX_KVM_GID")
		if _, err := strconv.ParseUint(gid, 10, 32); err != nil {
			t.Fatal("set OMNIGREX_MICROSANDBOX_KVM_GID to the Docker host's KVM device group")
		}
		p.wrapper = prefix + "-runtime"
		t.Cleanup(func() { p.cleanup("rm", "-f", "-v", p.wrapper) })
		p.command(time.Minute, nil, "run", "-d", "--rm", "--name", p.wrapper, "--network", p.network,
			"--label", "io.omnigrex.experiment=pi-feasibility",
			"--device", "/dev/kvm", "--group-add", gid, "--memory", "3g", "--memory-swap", "3g", "--cpus", "2", "--pids-limit", "512",
			"--env", "MSB_HOME=/tmp/msb", "--entrypoint", "/bin/sh", p.image, "-ec", "mkdir /tmp/state; exec sleep 1500")
		p.command(time.Minute, nil, "exec", p.wrapper, "msb", "doctor")
	}
	return p
}

func (p *probe) command(timeout time.Duration, input io.Reader, args ...string) string {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdin = input
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	if err != nil {
		p.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, p.safe(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func (p *probe) safe(value string) string {
	for _, secret := range p.secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	return value
}

func (p *probe) cleanup(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	if err != nil && !strings.Contains(string(output), "No such container") {
		p.t.Errorf("cleanup %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func (p *probe) control(path string, payload map[string]any) map[string]any {
	p.t.Helper()
	args := []string{"exec", p.server, "node", "/opt/probe/fixtures/control.mjs", path}
	if payload != nil {
		content, err := json.Marshal(payload)
		if err != nil {
			p.t.Fatal(err)
		}
		args = append(args, string(content))
	}
	output := p.command(30*time.Second, nil, args...)
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		p.t.Fatalf("invalid fixture control response: %v", err)
	}
	return result
}

type turn struct {
	p      *probe
	client *rpcClient
	name   string
	closed bool
}

func (p *probe) startTurn(sessionID, token string) *turn {
	p.t.Helper()
	return p.launch(sessionID, token, "", "", nil)
}

func (p *probe) launch(sessionID, token, provider, model string, auth []byte) *turn {
	p.t.Helper()
	name := "turn-" + strings.ToLower(rand.Text())
	environment := []string{"PROBE_SERVER_URL=" + p.serverURL, "PROBE_SESSION_ID=" + sessionID, "PROBE_MCP_TOKEN=" + token}
	if auth != nil {
		environment = append(environment, "PROBE_PROVIDER="+provider, "PROBE_MODEL="+model, "PROBE_LIVE_AUTH=/run/pi-agent/live-auth.json")
	}
	var command *exec.Cmd
	if p.backend == "docker" {
		args := []string{"create", "--name", name, "--network", p.network, "--read-only",
			"--label", "io.omnigrex.experiment=pi-feasibility",
			"--memory", "1g", "--cpus", "1", "--pids-limit", "128",
			"--mount", "type=volume,src=" + p.state + ",dst=/state",
			"--tmpfs", "/tmp:rw,uid=10001,gid=10001,mode=0700",
			"--tmpfs", "/workspace:rw,uid=10001,gid=10001,mode=0700",
			"--tmpfs", "/run/pi-agent:rw,uid=10001,gid=10001,mode=0700", "-i"}
		if p.captureWorkspace {
			if p.workspaceVolume == "" {
				p.workspaceVolume = p.state + "-workspace"
				p.command(time.Minute, nil, "volume", "create", p.workspaceVolume)
				p.t.Cleanup(func() { p.cleanup("volume", "rm", p.workspaceVolume) })
			}
			// A separately owned mount can be inspected without executing guest code.
			for i, arg := range args {
				if arg == "/workspace:rw,uid=10001,gid=10001,mode=0700" {
					args[i-1], args[i] = "--mount", "type=volume,src="+p.workspaceVolume+",dst=/workspace"
				}
			}
		}
		for _, entry := range environment {
			args = append(args, "--env", entry)
		}
		args = append(args, p.image)
		p.t.Cleanup(func() { p.cleanup("rm", "-f", "-v", name) })
		p.command(time.Minute, nil, args...)
		command = exec.CommandContext(p.ctx, "docker", "start", "-a", "-i", name)
	} else {
		args := []string{"exec", p.wrapper, "msb", "create", "--name", name,
			"--cpus", "1", "--memory", "1G", "--max-duration", "15m", "--user", "10001:10001",
			"--net-rule", "allow@" + strings.TrimSuffix(strings.TrimPrefix(p.serverURL, "http://"), ":8080"),
			"--mount-dir", "/tmp/state:/state:rw", "--copy-dir", "/opt/probe:/opt/probe",
			"--mkdir", "/workspace", "--mkdir", "/run/pi-agent", "--workdir", "/workspace",
			"--tmpfs", "/workspace:16M",
			"--tmpfs", "/run/pi-agent:16M",
			"--env", "HOME=/tmp", "--env", "PI_CODING_AGENT_DIR=/run/pi-agent", "--env", "PI_OFFLINE=1",
			"--env", "PI_TELEMETRY=0", "--env", "PI_SKIP_VERSION_CHECK=1"}
		if p.captureWorkspace {
			p.command(time.Minute, nil, "exec", p.wrapper, "mkdir", "/tmp/workspace")
			for i, arg := range args {
				if arg == "/workspace:16M" {
					args[i-1], args[i] = "--mount-dir", "/tmp/workspace:/workspace:rw"
				}
			}
		}
		for _, entry := range environment {
			args = append(args, "--env", entry)
		}
		if auth != nil {
			args = append(args, "--net", "public")
		} else {
			args = append(args, "--no-net")
		}
		args = append(args, nodeImage)
		p.command(5*time.Minute, nil, args...)
		p.command(time.Minute, nil, "exec", p.wrapper, "msb", "exec", "--no-stdin", "--user", "0:0", name, "--", "node", "-e",
			`for(const p of ['/workspace','/run/pi-agent']){require('fs').chownSync(p,10001,10001);require('fs').chmodSync(p,0o700);}`)
		command = exec.CommandContext(p.ctx, "docker", "exec", "-i", p.wrapper, "msb", "exec", "--stream", name, "--", "/bin/sh", "/opt/probe/fixtures/start.sh")
	}
	command.WaitDelay = 5 * time.Second
	client, err := startRPC(command)
	if err != nil {
		p.t.Fatal(err)
	}
	t := &turn{p: p, client: client, name: name}
	p.t.Cleanup(func() {
		if !t.closed {
			t.close()
		}
	})
	if auth != nil {
		if p.backend == "docker" {
			for attempts := 0; ; attempts++ {
				if p.command(10*time.Second, nil, "inspect", "--format", "{{.State.Running}}", name) == "true" {
					break
				}
				if attempts == 20 {
					p.t.Fatal("live credential destination did not start")
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		args := []string{"exec", "-i"}
		if p.backend == "docker" {
			args = append(args, name)
		} else {
			args = append(args, p.wrapper, "msb", "exec", "--stream", name, "--")
		}
		args = append(args, "node", "-e", `let b=[];process.stdin.on('data',c=>b.push(c));process.stdin.on('end',()=>require('fs').writeFileSync('/run/pi-agent/live-auth.json',Buffer.concat(b),{mode:0o600}));`)
		// Credentials travel only through stdin into disposable storage, never Docker env or arguments.
		p.command(time.Minute, bytes.NewReader(auth), args...)
	}
	return t
}

func (t *turn) close() {
	t.p.t.Helper()
	if err := t.client.close(); err != nil {
		t.p.t.Errorf("close Pi transport: %v\n%s", err, t.p.safe(t.client.stderr.String()))
	}
	if t.p.backend == "microsandbox" {
		t.p.command(time.Minute, nil, "exec", t.p.wrapper, "msb", "rm", "-f", t.name)
	}
	t.closed = true
}

func (t *turn) crash() {
	t.p.t.Helper()
	if t.p.backend == "docker" {
		t.p.command(time.Minute, nil, "kill", t.name)
	} else {
		t.p.command(time.Minute, nil, "exec", t.p.wrapper, "msb", "rm", "-f", t.name)
	}
	select {
	case <-t.client.done:
	case <-time.After(10 * time.Second):
		t.p.t.Fatal("transport survived destruction of its execution environment")
	}
	_ = t.client.stdin.Close()
	t.closed = true
}

func (t *turn) request(kind string, fields map[string]any) map[string]any {
	t.p.t.Helper()
	ctx, cancel := context.WithTimeout(t.p.ctx, 30*time.Second)
	defer cancel()
	response, err := t.client.request(ctx, kind, fields)
	if err != nil {
		t.p.t.Fatalf("%s: %v\n%s", kind, err, t.p.safe(t.client.stderr.String()))
	}
	return response
}

func (t *turn) success(kind string, fields map[string]any) map[string]any {
	t.p.t.Helper()
	response := t.request(kind, fields)
	if response["success"] != true {
		t.p.t.Fatalf("%s failed: %s", kind, t.p.safe(fmt.Sprint(response["error"])))
	}
	data, _ := response["data"].(map[string]any)
	return data
}

func (t *turn) settled(start int) {
	t.p.t.Helper()
	ctx, cancel := context.WithTimeout(t.p.ctx, 30*time.Second)
	defer cancel()
	if _, err := t.client.wait(ctx, start, func(record map[string]any) bool { return record["type"] == "agent_settled" }); err != nil {
		t.p.t.Fatalf("wait for agent_settled: %v\n%s", err, t.p.safe(t.client.stderr.String()))
	}
}

func (t *turn) ready() bool {
	commands, _ := json.Marshal(t.success("get_commands", nil))
	return bytes.Contains(commands, []byte(`"name":"omnigrex-probe-ready"`))
}

func (t *turn) audit() {
	t.p.t.Helper()
	args := []string{"exec"}
	if t.p.backend == "docker" {
		args = append(args, t.name)
	} else {
		args = append(args, t.p.wrapper, "msb", "exec", "--no-stdin", t.name, "--")
	}
	args = append(args, "node", "/opt/probe/fixtures/audit.mjs")
	var result struct {
		Files          int  `json:"files"`
		CredentialLeak bool `json:"credentialLeak"`
		PoisonExecuted bool `json:"poisonExecuted"`
	}
	if err := json.Unmarshal([]byte(t.p.command(time.Minute, nil, args...)), &result); err != nil {
		t.p.t.Fatal(err)
	}
	if result.Files == 0 || result.CredentialLeak || result.PoisonExecuted {
		t.p.t.Fatalf("persisted state/discovery audit failed: %+v", result)
	}
}

func (p *probe) waitForHeldRequest() {
	p.t.Helper()
	for range 20 {
		if p.control("/observations", nil)["held"].(float64) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatal("controlled provider did not receive the held request")
}

func lastStopReason(t *turn) string {
	messages := t.success("get_messages", nil)["messages"].([]any)
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index].(map[string]any)
		if message["role"] == "assistant" {
			reason, _ := message["stopReason"].(string)
			return reason
		}
	}
	return ""
}

func TestPiRPCFeasibility(t *testing.T) {
	p := newProbe(t)
	first := p.startTurn("participant-probe", "fixture-turn-one")
	if !first.ready() {
		t.Fatal("authenticated MCP extension is not ready")
	}
	state := first.success("get_state", nil)
	if state["sessionId"] != "participant-probe" {
		t.Fatalf("session identity = %v", state["sessionId"])
	}
	if response := first.request("set_model", map[string]any{"provider": "probe", "modelId": "does-not-exist"}); response["success"] != false {
		t.Fatal("missing model was not rejected")
	}
	levels := first.success("get_available_thinking_levels", nil)
	if fmt.Sprint(levels["levels"]) != "[off]" {
		t.Fatalf("non-reasoning model thinking levels = %v", levels)
	}
	if err := first.setThinkingStrict("high"); err == nil {
		t.Fatal("the controller accepted an unsupported thinking selection")
	}
	commands := first.success("get_commands", nil)
	encoded, _ := json.Marshal(commands)
	if !bytes.Contains(encoded, []byte("skill:approved")) || bytes.Contains(encoded, []byte("poison")) {
		t.Fatalf("explicit skill or discovery exclusion failed: %s", encoded)
	}

	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "hold"}}})
	start := first.client.mark()
	admission := first.success("prompt", map[string]any{"message": "remember-history-sentinel"})
	if admission["disposition"] != "started" || first.success("get_state", nil)["isStreaming"] != true {
		t.Fatal("prompt admission was not distinguishable from running work")
	}
	p.waitForHeldRequest()
	p.control("/release", map[string]any{})
	first.settled(start)
	if text := first.success("get_last_assistant_text", nil)["text"]; text != "fixture-complete\u2028unicode\u2029safe" {
		t.Fatalf("stream/framing result = %q", text)
	}
	entries := first.success("get_entries", nil)
	first.audit()
	first.close()

	before := len(p.control("/observations", nil)["requests"].([]any))
	second := p.startTurn("participant-probe", "fixture-turn-two")
	if !second.ready() {
		t.Fatal("fresh Runtime Process did not authenticate to MCP")
	}
	if second.success("get_state", nil)["sessionId"] != "participant-probe" {
		t.Fatal("fresh Runtime Process selected the wrong session")
	}
	replayed := second.success("get_entries", nil)
	oldJSON, _ := json.Marshal(entries["entries"])
	newJSON, _ := json.Marshal(replayed["entries"])
	if !bytes.Equal(oldJSON, newJSON) || !bytes.Contains(newJSON, []byte("remember-history-sentinel")) {
		t.Fatal("persisted-entry history changed or disappeared across fresh execution")
	}
	if after := len(p.control("/observations", nil)["requests"].([]any)); after != before {
		t.Fatal("History Replay caused a provider request")
	}

	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "tool", "value": "mcp-sentinel"}}})
	start = second.client.mark()
	second.success("prompt", map[string]any{"message": "call the gateway"})
	second.settled(start)
	observed := p.control("/observations", nil)
	encoded, _ = json.Marshal(observed)
	if !bytes.Contains(encoded, []byte("gateway:mcp-sentinel")) || !bytes.Contains(encoded, []byte("remember-history-sentinel")) {
		t.Fatal("MCP execution or continued model history was not observed")
	}
	for _, poison := range []string{"POISON_SYSTEM_CONTENT", "POISON_APPEND_CONTENT", "POISON_AUTOMATIC_CONTEXT_CONTENT", "POISON_PROMPT_CONTENT"} {
		if bytes.Contains(encoded, []byte(poison)) {
			t.Fatalf("uncontrolled resource was loaded: %s", poison)
		}
	}
	for _, request := range observed["requests"].([]any) {
		for _, tool := range request.(map[string]any)["tools"].([]any) {
			name := tool.(map[string]any)["function"].(map[string]any)["name"]
			if name != "read" && name != "probe_echo" {
				t.Fatalf("unexpected active tool: %v", name)
			}
		}
	}
	// Correlate simultaneous commands by ID, not by response ordering.
	var group sync.WaitGroup
	for _, kind := range []string{"get_state", "get_entries", "get_messages", "get_commands"} {
		group.Go(func() {
			ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
			defer cancel()
			response, err := second.client.request(ctx, kind, nil)
			if err != nil || response["command"] != kind || response["success"] != true {
				t.Errorf("correlated %s response failed: %v", kind, err)
			}
		})
	}
	group.Wait()

	// Pi clamps unsupported thinking levels. The future adapter must reject before setting.
	second.success("set_thinking_level", map[string]any{"level": "high"})
	if effective := second.success("get_state", nil)["thinkingLevel"]; effective != "off" {
		t.Fatalf("unexpected non-reasoning thinking behavior: %v", effective)
	}
	t.Log("OBSERVED: Pi acknowledges unsupported thinking and clamps it to off; validate against get_available_thinking_levels before prompting")

	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "error"}}})
	start = second.client.mark()
	second.success("prompt", map[string]any{"message": "fail after admission"})
	second.settled(start)
	if lastStopReason(second) != "error" {
		t.Fatal("provider error did not produce an error outcome")
	}

	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "hold"}}})
	start = second.client.mark()
	second.success("prompt", map[string]any{"message": "abort this request"})
	p.waitForHeldRequest()
	deadline, cancel := context.WithTimeout(p.ctx, 100*time.Millisecond)
	_, waitErr := second.client.wait(deadline, start, func(record map[string]any) bool { return record["type"] == "agent_settled" })
	cancel()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatal("a held prompt did not respect the client deadline")
	}
	second.success("abort", nil)
	second.settled(start)
	if lastStopReason(second) != "aborted" {
		t.Fatal("abort did not remain distinct from provider failure")
	}

	beforeRetry := len(p.control("/observations", nil)["requests"].([]any))
	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "error", "status": 503}, map[string]any{"kind": "text"}}})
	second.success("set_auto_retry", map[string]any{"enabled": true})
	start = second.client.mark()
	second.success("prompt", map[string]any{"message": "recover from one transient provider failure"})
	second.settled(start)
	if lastStopReason(second) != "stop" || len(p.control("/observations", nil)["requests"].([]any)) != beforeRetry+2 {
		t.Fatal("controlled transient failure did not settle after exactly one retry")
	}
	second.success("set_auto_retry", map[string]any{"enabled": false})

	beforeInvalid := len(p.control("/observations", nil)["mcpCalls"].([]any))
	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "tool", "arguments": map[string]any{}}}})
	start = second.client.mark()
	second.success("prompt", map[string]any{"message": "attempt invalid MCP arguments"})
	second.settled(start)
	if len(p.control("/observations", nil)["mcpCalls"].([]any)) != beforeInvalid {
		t.Fatal("invalid MCP arguments reached tool execution")
	}

	count := len(p.control("/observations", nil)["mcpCalls"].([]any))
	p.control("/configure", map[string]any{"revoked": true, "actions": []any{map[string]any{"kind": "tool"}}})
	start = second.client.mark()
	second.success("prompt", map[string]any{"message": "attempt a revoked MCP call"})
	second.settled(start)
	observed = p.control("/observations", nil)
	if len(observed["mcpCalls"].([]any)) != count || observed["rejected"].(float64) == 0 {
		t.Fatal("revoked MCP credentials executed a tool")
	}
	second.audit()
	second.close()
	p.control("/configure", map[string]any{})

	// Discard the creation response, then destroy execution before binding any returned ID.
	creation := p.startTurn("creation-recovery", "fixture-turn-one")
	if _, _, err := creation.client.send("get_state", nil); err != nil {
		t.Fatal(err)
	}
	if !creation.ready() {
		t.Fatal("creation probe did not finish startup")
	}
	creation.crash()
	recovered := p.startTurn("creation-recovery", "fixture-turn-two")
	if recovered.success("get_state", nil)["sessionId"] != "creation-recovery" {
		t.Fatal("ambiguous creation recovered the wrong identity")
	}
	recovered.close()

	// Losing the process after prompt admission is not a successful settlement.
	p.control("/configure", map[string]any{"actions": []any{map[string]any{"kind": "hold"}}})
	interrupted := p.startTurn("participant-probe", "fixture-turn-one")
	interrupted.success("prompt", map[string]any{"message": "interrupt after admission"})
	p.waitForHeldRequest()
	interrupted.crash()
	p.control("/configure", map[string]any{})
	recovered = p.startTurn("participant-probe", "fixture-turn-two")
	history, _ := json.Marshal(recovered.success("get_entries", nil))
	if !bytes.Contains(history, []byte("remember-history-sentinel")) {
		t.Fatal("process failure lost earlier persisted context")
	}
	recovered.close()

	missing := p.launch("missing-auth", "fixture-turn-one", "probe", "cheap", []byte("{}"))
	missing.success("get_state", nil)
	beforeMissing := len(p.control("/observations", nil)["requests"].([]any))
	start = missing.client.mark()
	response := missing.request("prompt", map[string]any{"message": "no provider credential"})
	if response["success"] == true {
		missing.settled(start)
		if lastStopReason(missing) != "error" {
			t.Fatal("missing provider credential did not fail clearly")
		}
	}
	if len(p.control("/observations", nil)["requests"].([]any)) != beforeMissing {
		t.Fatal("missing credential request reached the provider")
	}
	missing.close()

	bad := p.startTurn("unauthorized-probe", "invalid-credential")
	select {
	case <-bad.client.done:
	case <-time.After(20 * time.Second):
		t.Fatal("unauthenticated extension did not fail closed during startup")
	}
	if err := bad.client.close(); err == nil || !strings.Contains(bad.client.stderr.String(), "authenticated MCP preflight failed") {
		t.Fatal("unauthenticated extension did not produce the expected startup failure")
	}
	bad.closed = true
	if p.backend == "microsandbox" {
		p.command(time.Minute, nil, "exec", p.wrapper, "msb", "rm", "-f", bad.name)
	}
	t.Log("PASS: exact session reopening, replay without prompting, prompt admission, Unicode framing, authenticated MCP, explicit skills, and resource/tool exclusions")
}

func TestPiLiveProviders(t *testing.T) {
	path := os.Getenv("OMNIGREX_PI_LIVE_AUTH_FILE")
	if path == "" {
		t.Skip("live provider gate requires OMNIGREX_PI_LIVE_AUTH_FILE")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read the configured live credential file")
	}
	var bundle map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(content, &bundle); err != nil {
		t.Fatal("live credentials are not a JSON auth object")
	}
	for _, selection := range []struct{ provider, model string }{
		{"opencode-go", configuredModel("OMNIGREX_PI_OPENCODE_MODEL", "muse-spark-1.3-contributor")},
		{"openai", configuredModel("OMNIGREX_PI_OPENAI_MODEL", "o4-mini")},
	} {
		t.Run(selection.provider, func(t *testing.T) {
			credential, present := bundle[selection.provider]
			if !present || credential.Type != "api_key" || credential.Key == "" {
				t.Fatalf("configured file has no API-key credential for %s", selection.provider)
			}
			p := newProbe(t)
			p.secrets = append(p.secrets, credential.Key)
			auth, err := json.Marshal(map[string]any{selection.provider: credential})
			if err != nil {
				t.Fatal("cannot encode selected provider credential")
			}
			live := p.launch("live-probe", "fixture-turn-one", selection.provider, selection.model, auth)
			if !live.ready() {
				t.Fatal("live process MCP preflight did not complete")
			}
			state := live.success("get_state", nil)
			model := state["model"].(map[string]any)
			if model["provider"] != selection.provider || model["id"] != selection.model {
				t.Fatal("live model selection silently changed")
			}
			live.selectLowThinking()
			start := live.client.mark()
			live.success("prompt", map[string]any{"message": "Reply with exactly OK. Do not use tools."})
			live.settled(start)
			if lastStopReason(live) != "stop" {
				t.Fatalf("live provider did not complete normally: %s", live.failureDetail())
			}
			live.audit()
			live.close()
			t.Logf("PASS: live %s/%s authenticated response and credential-state exclusion", selection.provider, selection.model)
		})
	}
}

func (t *turn) failureDetail() string {
	messages := t.success("get_messages", nil)["messages"].([]any)
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index].(map[string]any)
		if message["role"] == "assistant" {
			return t.p.safe(fmt.Sprint(message["errorMessage"]))
		}
	}
	return "no assistant outcome"
}

// An opt-in subscription smoke test, not a qualification of durable OAuth refresh.
func TestPiSubscriptionSnapshot(t *testing.T) {
	path := os.Getenv("OMNIGREX_PI_OAUTH_AUTH_FILE")
	if path == "" {
		t.Skip("subscription snapshot test requires OMNIGREX_PI_OAUTH_AUTH_FILE")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read configured OAuth credential file")
	}
	var bundle map[string]struct {
		Type    string `json:"type"`
		Access  string `json:"access"`
		Expires int64  `json:"expires"`
	}
	if err := json.Unmarshal(content, &bundle); err != nil {
		t.Fatal("invalid OAuth credential file")
	}
	credential := bundle["openai"]
	if credential.Type != "oauth" || credential.Access == "" {
		t.Fatal("file has no OpenAI subscription credential")
	}
	if time.Until(time.UnixMilli(credential.Expires)) < 10*time.Minute {
		t.Fatal("subscription token is expired or near expiry; refresh it in Pi first; this probe never receives the real refresh token")
	}
	p := newProbe(t)
	p.secrets = append(p.secrets, credential.Access)
	// Withhold the actual refresh token and client ID, so this probe cannot rotate the user's credentials.
	auth, err := json.Marshal(map[string]any{"openai": map[string]any{
		"type": "oauth", "access": credential.Access, "expires": credential.Expires,
		"refresh": "refresh-intentionally-withheld",
	}})
	if err != nil {
		t.Fatal("cannot encode OAuth snapshot")
	}
	modelID := configuredModel("OMNIGREX_PI_OPENAI_MODEL", "o4-mini")
	live := p.launch("subscription-probe", "fixture-turn-one", "openai", modelID, auth)
	if !live.ready() {
		t.Fatal("subscription process MCP preflight failed")
	}
	model := live.success("get_state", nil)["model"].(map[string]any)
	if model["provider"] != "openai" || model["id"] != modelID {
		t.Fatal("subscription model selection changed")
	}
	live.selectLowThinking()
	start := live.client.mark()
	live.success("prompt", map[string]any{"message": "Reply with exactly OK. Do not use tools."})
	live.settled(start)
	if lastStopReason(live) != "stop" {
		t.Fatal("subscription snapshot did not complete normally; refresh is deliberately unavailable")
	}
	live.audit()
	live.close()
	t.Logf("PASS: OpenAI subscription access-token snapshot completed on %s without distributing refresh credentials", modelID)
}

func configuredModel(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func (t *turn) selectLowThinking() {
	t.p.t.Helper()
	levels := t.success("get_available_thinking_levels", nil)["levels"].([]any)
	if len(levels) == 0 {
		t.p.t.Fatal("model has no supported thinking levels")
	}
	selected := levels[0].(string)
	for _, level := range levels {
		if level == "low" {
			selected = "low"
		}
	}
	if err := t.setThinkingStrict(selected); err != nil {
		t.p.t.Fatal(err)
	}
}

func (t *turn) setThinkingStrict(selected string) error {
	levels := t.success("get_available_thinking_levels", nil)["levels"].([]any)
	for _, level := range levels {
		if level == selected {
			t.success("set_thinking_level", map[string]any{"level": selected})
			if t.success("get_state", nil)["thinkingLevel"] != selected {
				return errors.New("effective thinking level did not match the supported selection")
			}
			return nil
		}
	}
	return errors.New("requested thinking level is not supported by the selected model")
}
