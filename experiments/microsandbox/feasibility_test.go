//go:build integration

// Package microsandbox_test contains the Step 01 experiment, not a production backend.
package microsandbox_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// Microsandbox 0.7.6, Docker 29.7.2-dind, and Alpine 3.22.2 OCI indexes.
	runnerImage = "ghcr.io/superradcompany/microsandbox@sha256:be3d9f5f99b937ac4c4452b2ff33657a4ef74a117fdb584abb6d3c7db53e248f"
	dindImage   = "docker.io/library/docker@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6"
	alpineImage = "docker.io/library/alpine@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412"
)

//go:embed testdata
var fixtures embed.FS

func TestFixtureArchiveIncludesStandaloneModuleAndSource(t *testing.T) {
	source := []byte("committed-source-archive")
	content, err := fixtureArchive(source)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{
		"probe/guest.sh":                         false,
		"probe/compose.yaml":                     false,
		"probe/testcontainers/fixture.mod":       false,
		"probe/testcontainers/fixture.sum":       false,
		"probe/testcontainers/container_test.go": false,
		"probe/source.tar":                       false,
	}
	archive := tar.NewReader(bytes.NewReader(content))
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := required[header.Name]; !ok {
			continue
		}
		data, err := io.ReadAll(archive)
		if err != nil || len(data) == 0 {
			t.Fatalf("empty or unreadable fixture %s: %v", header.Name, err)
		}
		if header.Name == "probe/source.tar" && !bytes.Equal(data, source) {
			t.Fatal("source archive changed during packaging")
		}
		required[header.Name] = true
	}
	for name, present := range required {
		if !present {
			t.Errorf("missing fixture %s; embed excludes directories containing go.mod", name)
		}
	}
}

func TestCommittedSourceArchiveIncludesRepositoryRoot(t *testing.T) {
	p := &experiment{t: t, ctx: context.Background()}
	archive := tar.NewReader(bytes.NewReader(p.committedSource()))
	required := map[string]bool{"go.mod": false, "agent/pi/Dockerfile": false}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := required[header.Name]; ok {
			required[header.Name] = true
		}
	}
	for name, present := range required {
		if !present {
			t.Errorf("repository archive missing %s; git archive must run from the repository root", name)
		}
	}
}

func TestLinuxMicrosandboxDevelopmentEnvironment(t *testing.T) {
	if os.Getenv("OMNIGREX_MICROSANDBOX_FEASIBILITY") != "1" {
		t.Skip("set OMNIGREX_MICROSANDBOX_FEASIBILITY=1 on a Linux/amd64 Docker daemon with KVM; see README.md")
	}
	gid := os.Getenv("OMNIGREX_MICROSANDBOX_KVM_GID")
	parsedGID, err := strconv.ParseUint(gid, 10, 32)
	if err != nil || strconv.FormatUint(parsedGID, 10) != gid {
		t.Fatal("OMNIGREX_MICROSANDBOX_KVM_GID must be the numeric /dev/kvm group on the Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	probe := &experiment{t: t, ctx: ctx, container: "omnigrex-msb-feasibility-" + strings.ToLower(rand.Text())}
	platform := probe.run(30*time.Second, nil, "docker", "info", "--format", "{{.OSType}}/{{.Architecture}}")
	if platform != "linux/x86_64" && platform != "linux/amd64" {
		t.Fatalf("experiment requires a Linux/amd64 Docker daemon, got %q", platform)
	}
	t.Logf("Microsandbox 0.7.6 / SDK candidate v0.7.6; runner=%s; guest=%s", runnerImage, dindImage)
	probe.run(5*time.Minute, nil, "docker", "pull", runnerImage)
	// Register cleanup before creation: a lost create response must not orphan resources.
	t.Cleanup(probe.cleanup)
	probe.run(time.Minute, nil, "docker", "create", "--rm", "--name", probe.container,
		"--label", "io.omnigrex.experiment=microsandbox-feasibility",
		"--device", "/dev/kvm", "--user", "10001:10001", "--group-add", gid,
		"--memory", "6g", "--memory-swap", "6g", "--cpus", "2", "--pids-limit", "1024",
		"--env", "HOME=/tmp", "--env", "MSB_HOME=/tmp/microsandbox",
		"--entrypoint", "/bin/sh", runnerImage, "-ec", "exec sleep 2700")
	probe.run(time.Minute, nil, "docker", "start", probe.container)
	probe.exec(time.Minute, "msb", "doctor")
	probe.exec(time.Minute, "sh", "-ec", "test $(id -u) = 10001; test ! -S /var/run/docker.sock; mkdir /tmp/retained")

	if !t.Run("retained-state-survives-fresh-microvm", func(t *testing.T) {
		p := *probe
		p.t = t
		p.exec(5*time.Minute, "msb", "run", "--name", "state-first", "--no-stdin", "--no-tty",
			"--cpus", "1", "--memory", "256M", "--max-duration", "2m", "--timeout", "30s", "--no-net",
			"--user", "10001:10001", "--mount-dir", "/tmp/retained:/retained:rw", "--entrypoint", "/bin/sh", alpineImage,
			"--", "-ec", "test $(id -u) = 10001; printf retained > /retained/marker; touch /tmp/disposable")
		p.exec(time.Minute, "msb", "rm", "-f", "state-first")
		p.exec(5*time.Minute, "msb", "run", "--name", "state-second", "--no-stdin", "--no-tty",
			"--cpus", "1", "--memory", "256M", "--max-duration", "2m", "--timeout", "30s", "--no-net",
			"--user", "10001:10001", "--mount-dir", "/tmp/retained:/retained:rw", "--entrypoint", "/bin/sh", alpineImage,
			"--", "-ec", "test $(cat /retained/marker) = retained; test ! -e /tmp/disposable; test ! -S /var/run/docker.sock")
		p.exec(time.Minute, "msb", "rm", "-f", "state-second")
	}) {
		return
	}

	// Archive only the committed source. Never upload the working tree, .git, or local secrets.
	archive, err := fixtureArchive(probe.committedSource())
	if err != nil {
		t.Fatal(err)
	}
	probe.run(time.Minute, bytes.NewReader(archive), "docker", "cp", "-", probe.container+":/tmp")

	t.Run("docker-compose-testcontainers-and-omnigrex", func(t *testing.T) {
		p := *probe
		p.t = t
		p.exec(5*time.Minute, "msb", "create", "--name", "development", "--cpus", "2", "--memory", "4G",
			"--max-duration", "35m", "--root-disk", "flat:20G", "--net", "public",
			"--copy-dir", "/tmp/probe:/probe", "--entrypoint", "/bin/sh", dindImage)
		p.exec(30*time.Minute, "msb", "exec", "--no-stdin", "--timeout", "28m", "development", "--", "/bin/sh", "/probe/guest.sh")
		p.exec(time.Minute, "msb", "rm", "-f", "development")
	})
}

type experiment struct {
	t         *testing.T
	ctx       context.Context
	container string
}

func (p *experiment) committedSource() []byte {
	p.t.Helper()
	root := p.run(time.Minute, nil, "git", "rev-parse", "--show-toplevel")
	revision := p.run(time.Minute, nil, "git", "rev-parse", "HEAD")
	p.t.Logf("Omnigrex integration fixture revision: %s (committed source only)", revision)
	return p.output(time.Minute, nil, "git", "-C", root, "archive", "--format=tar", revision)
}

func (p *experiment) exec(timeout time.Duration, args ...string) string {
	p.t.Helper()
	return p.run(timeout, nil, "docker", append([]string{"exec", p.container}, args...)...)
}

func (p *experiment) run(timeout time.Duration, input io.Reader, program string, args ...string) string {
	p.t.Helper()
	return strings.TrimSpace(string(p.output(timeout, input, program, args...)))
}

func (p *experiment) output(timeout time.Duration, input io.Reader, program string, args ...string) []byte {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.WaitDelay = 5 * time.Second
	command.Stdin = input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		p.t.Fatalf("%s %s: %v (context: %v)\nstdout:\n%s\nstderr:\n%s", program, strings.Join(args, " "), err, ctx.Err(), output, stderr.Bytes())
	}
	if program != "git" || !slices.Contains(args, "archive") {
		p.t.Logf("%s %s\n%s%s", program, strings.Join(args, " "), output, stderr.Bytes())
	}
	return output
}

func (p *experiment) cleanup() {
	// Independent of the experiment deadline; deleting the wrapper also terminates its VMMs.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "rm", "--force", "--volumes", p.container)
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	if err != nil && !strings.Contains(string(output), "No such container") {
		p.t.Errorf("cleanup experiment container %s: %v\n%s", p.container, err, output)
	}
}

func fixtureArchive(source []byte) ([]byte, error) {
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	write := func(name string, content []byte, directory bool) error {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Uid: 10001, Gid: 10001}
		if directory {
			header.Mode, header.Typeflag, header.Size = 0o755, tar.TypeDir, 0
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		_, err := archive.Write(content)
		return err
	}
	if err := write("probe/", nil, true); err != nil {
		return nil, err
	}
	if err := fs.WalkDir(fixtures, "testdata", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "testdata" {
			return err
		}
		target := "probe/" + strings.TrimPrefix(name, "testdata/")
		if entry.IsDir() {
			return write(target+"/", nil, true)
		}
		content, err := fixtures.ReadFile(name)
		if err != nil {
			return err
		}
		return write(target, content, false)
	}); err != nil {
		return nil, fmt.Errorf("archive experiment fixtures: %w", err)
	}
	if err := write("probe/source.tar", source, false); err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
