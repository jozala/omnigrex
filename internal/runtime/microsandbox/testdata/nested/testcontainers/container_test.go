package fixture_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestPublishedPort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "docker.io/library/busybox@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092",
			ExposedPorts: []string{"8080/tcp"},
			Cmd:          []string{"sh", "-ec", "mkdir /www; printf sandbox-testcontainers-ok > /www/index.html; exec httpd -f -p 8080 -h /www"},
			WaitingFor:   wait.ForHTTP("/").WithPort("8080/tcp").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if container != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := container.Terminate(ctx); err != nil {
				t.Errorf("terminate fixture: %v", err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := container.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "sandbox-testcontainers-ok" {
		t.Fatalf("fixture response: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
}
