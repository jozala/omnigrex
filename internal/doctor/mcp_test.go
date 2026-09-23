package doctor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticMCPRejectsUnauthenticatedRequests(t *testing.T) {
	server := startTestDiagnosticMCP(t)
	response, err := http.Get(server.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("response = HTTP %d, WWW-Authenticate %q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
}

func TestDiagnosticMCPEnforcesProductionHTTPContract(t *testing.T) {
	server := startTestDiagnosticMCP(t)
	tests := []struct {
		name        string
		body        string
		accept      string
		contentType string
		status      int
	}{
		{name: "missing accept", body: `{}`, contentType: "application/json", status: http.StatusNotAcceptable},
		{name: "wrong content type", body: `{}`, accept: "application/json, text/event-stream", contentType: "text/plain", status: http.StatusUnsupportedMediaType},
		{name: "trailing JSON", body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}} {}`, accept: "application/json, text/event-stream", contentType: "application/json", status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL(), strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+server.Token())
			request.Header.Set("Accept", test.accept)
			request.Header.Set("Content-Type", test.contentType)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != test.status {
				t.Errorf("status = %d, want %d", response.StatusCode, test.status)
			}
		})
	}
}

func TestDiagnosticMCPRejectsIncompatibleProtocol(t *testing.T) {
	server := startTestDiagnosticMCP(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"old","capabilities":{},"clientInfo":{"name":"client","version":"1"}}}`)
	request, err := http.NewRequest(http.MethodPost, server.URL(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+server.Token())
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Contains(contents, []byte(`"error"`)) {
		t.Fatalf("response = HTTP %d %s", response.StatusCode, contents)
	}
}

func startTestDiagnosticMCP(t *testing.T) *diagnosticMCPServer {
	t.Helper()
	server, err := startDiagnosticMCP("localhost")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close diagnostic MCP server: %v", err)
		}
	})
	return server
}
