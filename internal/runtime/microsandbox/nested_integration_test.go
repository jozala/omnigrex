//go:build integration && microsandbox && cgo

package microsandbox

import (
	"bufio"
	"context"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMicrosandboxNestedDockerComposeAndTestcontainers(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	request.Image = "docker.io/library/docker@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6"
	request.MemoryBytes = 4 << 30
	request.PIDsLimit = 512
	request.Requirements = []execution.Requirement{execution.HardwareIsolation, execution.NestedContainers}
	request.Environment = []string{"HOME=/tmp", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_TLS_CERTDIR=", "GOMAXPROCS=2", "CGO_ENABLED=0", "GOTOOLCHAIN=local"}
	workspace := backend.location(request.Key, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", "testcontainers/fixture.mod", "testcontainers/fixture.sum", "testcontainers/container_test.go"} {
		content, err := os.ReadFile("/fixtures/" + name)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	request.Attachments = []execution.Attachment{{Resource: "workspace", Target: "/workspace"}}
	request.Workspace = execution.WorkspaceRef{Key: request.Key, Seed: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	request.Command = []string{"/bin/sh", "-ec", `test "$(id -u)" = 10001
id
cat /proc/self/cgroup
docker info --format 'guest CPUs={{.NCPU}}; engine={{.ServerVersion}}'
test "$(cat /proc/self/cgroup)" = '0::/microsandbox-workload/agent'
test "$(cat /sys/fs/cgroup/microsandbox-workload/cgroup.type)" = domain
test -z "$(cat /sys/fs/cgroup/microsandbox-workload/cgroup.procs)"
test "$(cat /sys/fs/cgroup/microsandbox-workload/pids.max)" = 512
test "$(docker info --format '{{.NCPU}}')" = 2
docker compose -p step08 -f /workspace/compose.yaml up --wait --wait-timeout 60 web
docker compose -p step08 -f /workspace/compose.yaml run --rm --no-deps client
docker compose -p step08 -f /workspace/compose.yaml down --volumes
wget -q https://go.dev/dl/go1.27.0.linux-amd64.tar.gz -O /tmp/go.tar.gz
printf '%s  /tmp/go.tar.gz\n' 675c26c449cbb18fc24b74650de1eabbae6e16f64326fd85a283fb3b58280685 | sha256sum -c -
tar -C /tmp -xzf /tmp/go.tar.gz
export PATH=/tmp/go/bin:$PATH
cd /workspace/testcontainers
cp fixture.mod go.mod; cp fixture.sum go.sum
docker pull docker.io/testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0
docker tag docker.io/testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0 testcontainers/ryuk:0.14.0
go test -count=1 -timeout 5m -v . >&2
docker run -d --name background docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092 sleep 600 >/dev/null
printf 'nested-qualified\n'
sleep 600
`}
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	engine, err := backend.New(request, backend.ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := engine.EnsureAbsent(cleanup); err != nil {
			t.Error(err)
		}
	})
	process, err := engine.Create(ctx, request, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Transport().Close() })
	if err := process.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(process.Transport())
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			observed, _ := process.ObserveExit(ctx)
			t.Fatalf("nested protocol: %v; observed=%+v", err, observed)
		}
		if line == "nested-qualified\n" {
			break
		}
		t.Log(line)
	}
	if err := process.Stop(ctx, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	actual, err := backend.inspect(ctx, nameFor(request.Key))
	if err != nil || actual.Status != "Stopped" {
		t.Fatalf("nested environment survived stop: %+v,%v", actual, err)
	}
	if err := process.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS: non-root agent, private guest Docker, guest cgroup budget, Compose, Testcontainers/Ryuk, and whole-VM termination with a running nested container")
}

func TestMicrosandboxNestedContainerDomainControllers(t *testing.T) {
	backend, request, ctx := nativeFixture(t)
	request.Image = "docker.io/library/docker@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6"
	request.MemoryBytes = 4 << 30
	request.PIDsLimit = 512
	request.Requirements = []execution.Requirement{execution.HardwareIsolation, execution.NestedContainers}
	request.Environment = []string{"HOME=/tmp", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_TLS_CERTDIR="}
	request.Command = []string{"/bin/sh", "-ec", `
test "$(cat /proc/self/cgroup)" = '0::/microsandbox-workload/agent'
test "$(cat /sys/fs/cgroup/microsandbox-workload/cgroup.type)" = domain
test -z "$(cat /sys/fs/cgroup/microsandbox-workload/cgroup.procs)"
test "$(cat /sys/fs/cgroup/microsandbox-workload/pids.max)" = 512
image=docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092
docker run --rm --memory 64m "$image" sh -ec 'test "$(cat /sys/fs/cgroup/memory.max)" = 67108864'
docker build --tag domain-controller-regression - <<'DOCKERFILE'
FROM docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092
RUN true
DOCKERFILE
docker run --rm --memory 64m domain-controller-regression sh -ec 'test "$(cat /sys/fs/cgroup/memory.max)" = 67108864'
docker run --detach --name domain-background --memory 64m "$image" sleep 300 >/dev/null
pid="$(docker inspect --format '{{.State.Pid}}' domain-background)"
case "$(cat "/proc/$pid/cgroup")" in
  0::/microsandbox-workload/agent|0::/microsandbox-workload/agent/*) exit 1 ;;
  0::/microsandbox-workload/*) ;;
  *) exit 1 ;;
esac
test "$(cat /sys/fs/cgroup/microsandbox-workload/cgroup.type)" = domain
test -z "$(cat /sys/fs/cgroup/microsandbox-workload/cgroup.procs)"
printf 'domain-controllers-ready\n'
sleep 300
`}
	if err := (commandCLI{backend.options}).Run(ctx, nil, io.Discard, "image", "pull", request.Image); err != nil {
		t.Fatal(err)
	}
	process := launchFixture(t, ctx, backend, request)
	interrupt := context.AfterFunc(ctx, func() { _ = process.Transport().Close() })
	defer interrupt()
	reader := bufio.NewReader(process.Transport())
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("nested domain qualification: %v", err)
		}
		if line == "domain-controllers-ready\n" {
			break
		}
		t.Log(line)
	}
	meta, err := backend.inspect(ctx, nameFor(request.Key))
	if err != nil {
		t.Fatal(err)
	}
	// Once domain controllers are delegated, further SDK execs must still be
	// placed in the agent leaf rather than the now-empty aggregate domain.
	if err := backend.platform.Execute(ctx, meta, []string{"/bin/sh", "-ec", `test "$(cat /proc/self/cgroup)" = '0::/microsandbox-workload/agent'; test "$(cat /sys/fs/cgroup/microsandbox-workload/pids.max)" = 512`}, "10001:10001", nil); err != nil {
		t.Fatalf("SDK exec after domain-controller delegation: %v", err)
	}
	if err := process.Stop(ctx, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := process.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS: memory limits before and after BuildKit, empty domain parent, shared PID ancestry, subsequent SDK exec, and whole-VM cleanup")
}
