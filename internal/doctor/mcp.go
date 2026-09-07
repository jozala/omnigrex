package doctor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jozala/omnigrex/internal/mcp"
)

const diagnosticMCPBodyLimit = 64 << 10

type diagnosticMCPServer struct {
	server       *http.Server
	listener     net.Listener
	token        string
	host         string
	ready        chan struct{}
	readyOnce    sync.Once
	mutex        sync.Mutex
	initializing bool
	initialized  bool
}

type diagnosticRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func startDiagnosticMCP(host string) (*diagnosticMCPServer, error) {
	if host == "" {
		return nil, errors.New("diagnostic MCP host is empty")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generate diagnostic MCP credential: %w", err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("start diagnostic MCP listener: %w", err)
	}
	server := &diagnosticMCPServer{
		listener: listener,
		token:    base64.RawURLEncoding.EncodeToString(tokenBytes),
		host:     host,
		ready:    make(chan struct{}),
	}
	server.server = &http.Server{Handler: server}
	go func() {
		_ = server.server.Serve(listener)
	}()
	return server, nil
}

func (server *diagnosticMCPServer) URL() string {
	port := server.listener.Addr().(*net.TCPAddr).Port
	return "http://" + net.JoinHostPort(server.host, strconv.Itoa(port)) + "/mcp"
}

func (server *diagnosticMCPServer) Token() string {
	return server.token
}

func (server *diagnosticMCPServer) Ready() <-chan struct{} {
	return server.ready
}

func (server *diagnosticMCPServer) Close(ctx context.Context) error {
	return server.server.Shutdown(ctx)
}

func (server *diagnosticMCPServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	expectedAuthorization := "Bearer " + server.token
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")), []byte(expectedAuthorization)) != 1 {
		response.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if request.Header.Get("Origin") != "" {
		http.Error(response, "origin is not allowed", http.StatusForbidden)
		return
	}
	if request.URL.Path != "/mcp" {
		http.NotFound(response, request)
		return
	}
	if request.Method == http.MethodGet {
		if !server.requestReady(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		if !acceptsDiagnosticMediaType(request.Header.Get("Accept"), "text/event-stream") {
			http.Error(response, "Accept must include text/event-stream", http.StatusNotAcceptable)
			return
		}
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.Method == http.MethodDelete {
		if !server.requestReady(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !acceptsDiagnosticMediaType(request.Header.Get("Accept"), "application/json") ||
		!acceptsDiagnosticMediaType(request.Header.Get("Accept"), "text/event-stream") {
		http.Error(response, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	if !hasDiagnosticMediaType(request.Header.Get("Content-Type"), "application/json") {
		http.Error(response, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, diagnosticMCPBodyLimit)
	decoder := json.NewDecoder(request.Body)
	var rpc diagnosticRPCRequest
	if err := decoder.Decode(&rpc); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(response, "invalid MCP request", status)
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF || rpc.JSONRPC != "2.0" || rpc.Method == "" {
		http.Error(response, "invalid MCP request", http.StatusBadRequest)
		return
	}
	if len(rpc.ID) != 0 && !validDiagnosticRequestID(rpc.ID) {
		writeDiagnosticRPC(response, json.RawMessage("null"), nil, map[string]any{"code": -32600, "message": "invalid request"})
		return
	}
	switch rpc.Method {
	case "initialize":
		server.initialize(response, request, rpc)
	case "notifications/initialized":
		server.completeInitialization(response, request, rpc)
	case "tools/list":
		server.listTools(response, request, rpc)
	default:
		if !server.requestReady(request) {
			http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
			return
		}
		if len(rpc.ID) == 0 {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		writeDiagnosticRPC(response, rpc.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
	}
}

func (server *diagnosticMCPServer) initialize(response http.ResponseWriter, request *http.Request, rpc diagnosticRPCRequest) {
	var params struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(rpc.Params)))
	decoder.DisallowUnknownFields()
	requestVersion := request.Header.Get("MCP-Protocol-Version")
	if decoder.Decode(&params) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(rpc.ID) == 0 ||
		(requestVersion != "" && requestVersion != mcp.ProtocolVersion) || params.ProtocolVersion != mcp.ProtocolVersion ||
		params.Capabilities == nil || params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
		writeDiagnosticRPC(response, rpc.ID, nil, map[string]any{"code": -32602, "message": "invalid initialize params"})
		return
	}
	server.mutex.Lock()
	server.initializing = true
	server.initialized = false
	server.mutex.Unlock()
	writeDiagnosticRPC(response, rpc.ID, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
		"serverInfo":      map[string]string{"name": "omnigrex-doctor", "version": "1.0.0"},
	}, nil)
}

func (server *diagnosticMCPServer) completeInitialization(response http.ResponseWriter, request *http.Request, rpc diagnosticRPCRequest) {
	server.mutex.Lock()
	valid := server.initializing && len(rpc.ID) == 0 && emptyDiagnosticParams(rpc.Params) && request.Header.Get("MCP-Protocol-Version") == mcp.ProtocolVersion
	if valid {
		server.initializing = false
		server.initialized = true
	}
	server.mutex.Unlock()
	if !valid {
		http.Error(response, "invalid MCP initialization", http.StatusBadRequest)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func (server *diagnosticMCPServer) listTools(response http.ResponseWriter, request *http.Request, rpc diagnosticRPCRequest) {
	server.mutex.Lock()
	valid := server.initialized && len(rpc.ID) != 0 && emptyDiagnosticParams(rpc.Params) && request.Header.Get("MCP-Protocol-Version") == mcp.ProtocolVersion
	server.mutex.Unlock()
	if !valid {
		http.Error(response, "MCP client is not initialized", http.StatusBadRequest)
		return
	}
	writeDiagnosticRPC(response, rpc.ID, map[string]any{"tools": []any{}}, nil)
	server.readyOnce.Do(func() { close(server.ready) })
}

func (server *diagnosticMCPServer) requestReady(request *http.Request) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.initialized && request.Header.Get("MCP-Protocol-Version") == mcp.ProtocolVersion
}

func validDiagnosticRequestID(raw json.RawMessage) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	switch id := value.(type) {
	case string:
		return true
	case json.Number:
		_, err := id.Int64()
		return err == nil
	default:
		return false
	}
}

func emptyDiagnosticParams(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var params map[string]json.RawMessage
	return json.Unmarshal(raw, &params) == nil && len(params) == 0
}

func acceptsDiagnosticMediaType(header, mediaType string) bool {
	for value := range strings.SplitSeq(header, ",") {
		parsed, parameters, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || !strings.EqualFold(parsed, mediaType) {
			continue
		}
		quality := 1.0
		if text, present := parameters["q"]; present {
			quality, err = strconv.ParseFloat(text, 64)
			if err != nil {
				continue
			}
		}
		if quality > 0 && quality <= 1 {
			return true
		}
	}
	return false
}

func hasDiagnosticMediaType(header, mediaType string) bool {
	parsed, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(parsed, mediaType)
}

func writeDiagnosticRPC(response http.ResponseWriter, id json.RawMessage, result, rpcError any) {
	response.Header().Set("Content-Type", "application/json")
	payload := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcError != nil {
		payload["error"] = rpcError
	} else {
		payload["result"] = result
	}
	_ = json.NewEncoder(response).Encode(payload)
}
