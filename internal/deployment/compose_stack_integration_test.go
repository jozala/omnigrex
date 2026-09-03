//go:build integration

package deployment

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestComposeFullStack(t *testing.T) {
	if os.Getenv("OMNIGREX_COMPOSE_INTEGRATION") != "1" {
		t.Skip("set OMNIGREX_COMPOSE_INTEGRATION=1 to allow destructive use of stable Compose resources")
	}

	secretDirectory := t.TempDir()
	databaseSecret := writeTestSecret(t, secretDirectory, "database-password", []byte("compose-integration-secret\n"))
	webhookSecret := writeTestSecret(t, secretDirectory, "webhook-secret", []byte("compose-webhook-secret\n"))
	developerPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate Developer GitHub App test key: %v", err)
	}
	reviewerPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate Reviewer GitHub App test key: %v", err)
	}
	developerPrivateKeyFile := writeTestSecret(t, secretDirectory, "github-developer-app.pem", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(developerPrivateKey),
	}))
	reviewerPrivateKeyFile := writeTestSecret(t, secretDirectory, "github-reviewer-app.pem", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(reviewerPrivateKey),
	}))
	dockerGID := os.Getenv("OMNIGREX_DOCKER_GID")
	if dockerGID == "" {
		dockerGID = "0"
	}
	httpPort := freeHTTPPort(t)
	fixture := composeFixture{
		root: repositoryRoot(t),
		environment: composeEnvironment(databaseSecret, dockerGID, strconv.Itoa(os.Getgid()), httpPort, map[string]string{
			"OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE": developerPrivateKeyFile,
			"OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE":  reviewerPrivateKeyFile,
			"OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE":        webhookSecret,
		}),
	}
	t.Cleanup(func() { fixture.captureLogsAndClean(t) })

	fixture.mustRun(t, 2*time.Minute, "down", "--volumes", "--remove-orphans", "--timeout", "10")
	fixture.mustRun(t, 15*time.Minute, "up", "--build", "--wait")

	baseURL := "http://127.0.0.1:" + httpPort
	waitForHTTPStatus(t, baseURL+"/healthz", http.StatusOK, 30*time.Second)
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 30*time.Second)
	fixture.verifyDurableWebhook(t, baseURL)
	fixture.agentVolumes(t, `set -eu
for path in /workspace /home/opencode/.local/share/opencode /home/opencode/.local/share/mise; do
  printf persistent > "$path/.omnigrex-compose-marker"
done`)
	fixture.mustRun(t, 5*time.Minute, "up", "--detach", "--force-recreate", "--no-deps", "--wait", "orchestrator")
	fixture.agentVolumes(t, `set -eu
for path in /workspace /home/opencode/.local/share/opencode /home/opencode/.local/share/mise; do
  test "$(cat "$path/.omnigrex-compose-marker")" = persistent
done`)
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 90*time.Second)
	fixture.assertPostgresIsolatedFromAgentNetwork(t)

	marker := "compose-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	fixture.psql(t, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS compose_integration_markers (marker TEXT PRIMARY KEY); INSERT INTO compose_integration_markers (marker) VALUES ('%s');",
		marker,
	))

	fixture.mustRun(t, 5*time.Minute, "up", "--detach", "--force-recreate", "--no-deps", "--wait", "postgres")
	gotMarker := fixture.psql(t, fmt.Sprintf(
		"SELECT marker FROM compose_integration_markers WHERE marker = '%s';",
		marker,
	))
	if gotMarker != marker {
		t.Fatalf("durable PostgreSQL marker after container recreation = %q, want %q", gotMarker, marker)
	}
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 90*time.Second)

	fixture.mustRun(t, 2*time.Minute, "stop", "--timeout", "10", "postgres")
	assertHTTPStatus(t, baseURL+"/healthz", http.StatusOK)
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusServiceUnavailable, 45*time.Second)
	assertHTTPStatus(t, baseURL+"/healthz", http.StatusOK)

	fixture.mustRun(t, 2*time.Minute, "start", "postgres")
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK, 90*time.Second)
}

func writeTestSecret(t *testing.T, directory, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, contents, 0o640); err != nil {
		t.Fatalf("write Compose test secret %q: %v", name, err)
	}
	return path
}

type composeFixture struct {
	root        string
	environment []string
}

func (fixture composeFixture) mustRun(t *testing.T, timeout time.Duration, arguments ...string) string {
	t.Helper()
	output, err := executeCompose(fixture.root, fixture.environment, timeout, arguments...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func (fixture composeFixture) psql(t *testing.T, query string) string {
	t.Helper()
	return fixture.mustRun(t, time.Minute,
		"exec", "-T", "postgres",
		"psql", "--no-psqlrc", "--quiet", "--tuples-only", "--no-align",
		"--set", "ON_ERROR_STOP=1", "--username", "omnigrex", "--dbname", "omnigrex",
		"--command", query,
	)
}

func (fixture composeFixture) verifyDurableWebhook(t *testing.T, baseURL string) {
	t.Helper()
	const (
		deliveryID = "123e4567-e89b-42d3-a456-426614174000"
		secret     = "compose-webhook-secret"
	)
	body := []byte(`{"action":"closed","repository":{"id":9123,"name":"omnigrex","owner":{"login":"jozala"}},"issue":{"id":456,"number":12}}`)
	for range 2 {
		if status := postWebhook(t, baseURL, deliveryID, "issues", secret, body); status != http.StatusAccepted {
			t.Fatalf("valid webhook status = %d, want %d", status, http.StatusAccepted)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state := fixture.psql(t, fmt.Sprintf(`
SELECT delivery.status || ':' || count(event.delivery_id)::text
FROM webhook_deliveries AS delivery
LEFT JOIN normalized_events AS event ON event.delivery_id = delivery.delivery_id
WHERE delivery.delivery_id = '%s'
GROUP BY delivery.status`, deliveryID))
		if state == "PROCESSED:1" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state := fixture.psql(t, fmt.Sprintf(`
SELECT delivery.status || ':' || count(event.delivery_id)::text
FROM webhook_deliveries AS delivery
LEFT JOIN normalized_events AS event ON event.delivery_id = delivery.delivery_id
WHERE delivery.delivery_id = '%s'
GROUP BY delivery.status`, deliveryID)); state != "PROCESSED:1" {
		t.Fatalf("durable webhook state = %q, want PROCESSED:1", state)
	}
	const unsupportedID = "323e4567-e89b-42d3-a456-426614174000"
	if status := postWebhook(t, baseURL, unsupportedID, "ping", secret, []byte(`not JSON`)); status != http.StatusAccepted {
		t.Fatalf("unsupported webhook status = %d, want %d", status, http.StatusAccepted)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if state := fixture.psql(t, fmt.Sprintf(`SELECT status FROM webhook_deliveries WHERE delivery_id = '%s'`, unsupportedID)); state == "IGNORED" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state := fixture.psql(t, fmt.Sprintf(`SELECT status FROM webhook_deliveries WHERE delivery_id = '%s'`, unsupportedID)); state != "IGNORED" {
		t.Fatalf("unsupported webhook state = %q, want IGNORED", state)
	}
	if count := fixture.psql(t, fmt.Sprintf(`SELECT count(*) FROM normalized_events WHERE delivery_id = '%s'`, unsupportedID)); count != "0" {
		t.Fatalf("unsupported normalized events = %q, want 0", count)
	}

	if status := postWebhook(t, baseURL, "223e4567-e89b-42d3-a456-426614174000", "issues", "wrong-secret", body); status != http.StatusUnauthorized {
		t.Fatalf("invalid webhook status = %d, want %d", status, http.StatusUnauthorized)
	}
	if count := fixture.psql(t, `SELECT count(*) FROM webhook_deliveries WHERE delivery_id = '223e4567-e89b-42d3-a456-426614174000'`); count != "0" {
		t.Fatalf("invalid webhook rows = %q, want 0", count)
	}
}

func postWebhook(t *testing.T, baseURL, deliveryID, eventName, secret string, body []byte) int {
	t.Helper()
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(body)
	request, err := http.NewRequest(http.MethodPost, baseURL+"/webhooks/github", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create webhook request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-GitHub-Event", eventName)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(digest.Sum(nil)))
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("send webhook request: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func (fixture composeFixture) agentVolumes(t *testing.T, script string) {
	t.Helper()
	fixture.mustRun(t, 2*time.Minute,
		"run", "--rm", "--no-deps", "--entrypoint", "sh", "opencode-image", "-c", script,
	)
}

func (fixture composeFixture) assertPostgresIsolatedFromAgentNetwork(t *testing.T) {
	t.Helper()
	postgresIP := fixture.mustRunDocker(t, time.Minute,
		"inspect", "--format", `{{(index .NetworkSettings.Networks "omnigrex-backend").IPAddress}}`,
		"omnigrex-postgres-1",
	)
	fixture.mustRunDocker(t, time.Minute,
		"run", "--rm", "--network", "omnigrex-agent", "--user", "10001:10001",
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--entrypoint", "sh", readinessImage, "-c",
		`if nc -z -w 2 "$1" 5432; then exit 1; fi`, "sh", postgresIP,
	)
}

func (fixture composeFixture) mustRunDocker(t *testing.T, timeout time.Duration, arguments ...string) string {
	t.Helper()
	output, err := executeDocker(fixture.root, fixture.environment, timeout, arguments...)
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func (fixture composeFixture) captureLogsAndClean(t *testing.T) {
	t.Helper()
	if t.Failed() {
		output, err := executeCompose(fixture.root, fixture.environment, time.Minute, "logs", "--no-color", "--timestamps")
		if err != nil {
			t.Logf("capture Docker Compose logs: %v\n%s", err, output)
		} else {
			t.Logf("Docker Compose logs:\n%s", output)
		}
	}
	output, err := executeCompose(
		fixture.root,
		fixture.environment,
		2*time.Minute,
		"down", "--volumes", "--remove-orphans", "--timeout", "10",
	)
	if err != nil {
		t.Errorf("clean Docker Compose resources: %v\n%s", err, output)
	}
}

func freeHTTPPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("choose free HTTP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release free HTTP port: %v", err)
	}
	return strconv.Itoa(port)
}

func waitForHTTPStatus(t *testing.T, url string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastResult := "no request made"
	for time.Now().Before(deadline) {
		status, body, err := getHTTPStatus(url)
		if err == nil && status == want {
			return
		}
		if err != nil {
			lastResult = err.Error()
		} else {
			lastResult = fmt.Sprintf("%s with body %q", http.StatusText(status), body)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("GET %s did not return %d within %s; last result: %s", url, want, timeout, lastResult)
}

func assertHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()
	status, body, err := getHTTPStatus(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if status != want {
		t.Fatalf("GET %s status = %d with body %q, want %d", url, status, body, want)
	}
}

func getHTTPStatus(url string) (int, string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if readErr != nil {
		return response.StatusCode, "", readErr
	}
	return response.StatusCode, strings.TrimSpace(string(body)), nil
}
