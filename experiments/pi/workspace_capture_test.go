//go:build integration

package pi_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This is a mechanism proof, not the production capture encoding.
// The copy is performed by the trusted controller while all guest writers are
// suspended, with Pi blocked on an authenticated, still-outstanding MCP call.
func TestPiMidPromptWorkspaceCapture(t *testing.T) {
	p := newProbe(t)
	p.captureWorkspace = true
	live := p.startTurn("workspace-capture", "fixture-turn-one")
	if !live.ready() {
		t.Fatal("authenticated MCP extension is not ready")
	}
	writerArgs := []string{"exec", "-d", live.name}
	if p.backend == "microsandbox" {
		writerArgs = []string{"exec", "-d", p.wrapper, "msb", "exec", "--no-stdin", live.name, "--"}
	}
	p.command(time.Minute, nil, append(writerArgs, "node", "/opt/probe/fixtures/workspace-writer.mjs")...)
	p.control("/configure", map[string]any{"holdTools": true, "actions": []any{map[string]any{"kind": "tool", "value": "capture-outstanding"}}})
	start := live.client.mark()
	live.success("prompt", map[string]any{"message": "call the gateway and wait for its result"})
	for attempts := 0; ; attempts++ {
		if p.control("/observations", nil)["heldTools"].(float64) == 1 {
			break
		}
		if attempts == 40 {
			t.Fatal("MCP call was not held")
		}
		time.Sleep(100 * time.Millisecond)
	}
	before := live.workspaceGeneration()
	time.Sleep(100 * time.Millisecond)
	if before == live.workspaceGeneration() {
		t.Fatal("background workspace writer did not advance before suspension")
	}
	paused := false
	t.Cleanup(func() {
		if paused {
			live.resumeWorkspace()
		}
	})
	live.pauseWorkspace()
	paused = true
	sealed := live.workspaceArchive()
	time.Sleep(150 * time.Millisecond)
	if second := live.workspaceArchive(); !bytes.Equal(sealed, second) {
		t.Fatal("workspace changed while the backend claimed writers were suspended")
	}
	verifyWorkspaceArchive(t, sealed)
	if p.control("/observations", nil)["heldTools"].(float64) != 1 {
		t.Fatal("capture consumed or disconnected the outstanding MCP call")
	}
	live.resumeWorkspace()
	paused = false
	if live.success("get_state", nil)["isStreaming"] != true {
		t.Fatal("suspension ended the outstanding prompt")
	}
	p.control("/release-tools", nil)
	live.settled(start)
	if lastStopReason(live) != "stop" {
		t.Fatal("RPC/MCP channel did not remain usable after capture")
	}
	time.Sleep(100 * time.Millisecond)
	if before == live.workspaceGeneration() {
		t.Fatal("workspace writer did not resume")
	}
	live.crash()
	verifyWorkspaceArchive(t, sealed)
	t.Logf("PASS: %s mid-prompt capture, stable suspended writers, modes/symlinks, MCP/RPC continuation, retained bytes sha256=%x", p.backend, sha256.Sum256(sealed))
}

func (live *turn) workspaceGeneration() string {
	args := []string{"exec", live.name}
	path := "/workspace/evidence/generation"
	if live.p.backend == "microsandbox" {
		args = []string{"exec", live.p.wrapper}
		path = "/tmp/workspace/evidence/generation"
	}
	return live.p.command(30*time.Second, nil, append(args, "node", "-e", "console.log(require('fs').readFileSync('"+path+"')[0])")...)
}

func (live *turn) pauseWorkspace() {
	if live.p.backend == "docker" {
		live.p.command(30*time.Second, nil, "pause", live.name)
		if live.p.command(time.Minute, nil, "inspect", "--format", "{{.State.Paused}}", live.name) != "true" {
			live.p.t.Fatal("Docker did not confirm suspension")
		}
	} else {
		live.p.command(30*time.Second, nil, "exec", live.p.wrapper, "msb", "pause", live.name, "--guest-flush", "required")
	}
}

func (live *turn) resumeWorkspace() {
	if live.p.backend == "docker" {
		live.p.command(30*time.Second, nil, "unpause", live.name)
	} else {
		live.p.command(30*time.Second, nil, "exec", live.p.wrapper, "msb", "resume", live.name)
	}
}

func (live *turn) workspaceArchive() []byte {
	live.p.t.Helper()
	source := live.name + ":/workspace/evidence"
	if live.p.backend == "microsandbox" {
		source = live.p.wrapper + ":/tmp/workspace/evidence"
	}
	ctx, cancel := context.WithTimeout(live.p.ctx, 30*time.Second)
	defer cancel()
	args := []string{"cp", source, "-"}
	if live.p.backend == "microsandbox" {
		args = []string{"exec", live.p.wrapper, "tar", "--format=pax", "--sort=name", "--xattrs", "--xattrs-include=user.msb.override_stat",
			"--pax-option=delete=atime,delete=ctime", "-cf", "-", "-C", "/tmp/workspace", "evidence"}
	}
	command := exec.CommandContext(ctx, "docker", args...)
	command.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		live.p.t.Fatalf("trusted workspace transfer: %v: %s", err, stderr.String())
	}
	return output
}

func verifyWorkspaceArchive(t *testing.T, content []byte) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(content))
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(header.Name, "evidence/")
		// Microsandbox 0.7.6 passthroughfs stores guest stat in a 20-byte
		// versioned xattr; host modes and file-backed links are not guest stat.
		if override, exists := header.PAXRecords["SCHILY.xattr.user.msb.override_stat"]; exists {
			if len(override) != 20 || override[0] != 1 {
				t.Fatal("unrecognized Microsandbox stat metadata")
			}
			mode := binary.LittleEndian.Uint32([]byte(override)[12:16])
			header.Mode = int64(mode & 0o777)
			switch mode & 0o170000 {
			case 0o100000:
				header.Typeflag = tar.TypeReg
			case 0o040000:
				header.Typeflag = tar.TypeDir
			case 0o120000:
				link, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				header.Typeflag, header.Linkname = tar.TypeSymlink, string(link)
			default:
				t.Fatal("unsupported guest entry type")
			}
		}
		seen[name] = true
		switch name {
		case "generation":
			data, err := io.ReadAll(reader)
			if err != nil || len(data) != 1024*1024 || !bytes.Equal(data, bytes.Repeat(data[:1], len(data))) {
				t.Fatal("captured writer generation is torn or incomplete")
			}
		case "executable":
			if header.Typeflag != tar.TypeReg || header.Mode&0o111 == 0 {
				t.Fatalf("capture lost executable mode: type=%d mode=%#o", header.Typeflag, header.Mode)
			}
		case "link":
			if header.Typeflag != tar.TypeSymlink || header.Linkname != "executable" {
				t.Fatal("capture lost symlink identity")
			}
		}
	}
	for _, name := range []string{"generation", "executable", "link"} {
		if !seen[name] {
			t.Fatalf("capture omitted %s", name)
		}
	}
}
