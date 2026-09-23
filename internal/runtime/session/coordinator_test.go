package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
)

func TestPrepareCreatesAndBindsNewSessionUnderTurnFence(t *testing.T) {
	operations := []string{}
	servers := []acp.MCPServer{{Name: "tools", Command: "/usr/local/bin/tools", Args: []string{"serve"}}}
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	client := &agentClient{
		operations:          &operations,
		recordEventContexts: true,
		initialize:          acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"sessionCapabilities":{"resume":{},"list":{}},"z":true}`)},
		recoverErr:          acp.ErrSessionNotFound,
		created:             acp.Session{ID: "acp-new", ConfigOptions: configOptions("old/model", "low", "build")},
		configResponses: [][]json.RawMessage{
			configOptions("provider/model", "low", "build"),
			configOptions("provider/model", "high", "build"),
			configOptions("provider/model", "high", "omnigrex-developer"),
		},
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	result, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease:         lease,
		Session:       durable,
		Client:        client,
		StatePath:     durable.RuntimeStatePath,
		MCPServers:    servers,
		Configuration: opencode.SessionConfiguration{Model: "provider/model", Variant: "high", Mode: "omnigrex-developer"},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.AgentSessionID != durable.ID || result.ACPSessionID != "acp-new" || result.Disposition != session.Created {
		t.Fatalf("Prepare() result = %#v", result)
	}
	wantOperations := []string{
		"fence", "event-context:", "initialize", "fence", "recover:/workspace", "fence", "create",
		"fence", "bind:acp-new", "event-context:acp-new", "fence", "config:model", "fence", "config:effort", "fence", "config:mode",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
	if !reflect.DeepEqual(client.createRequest.MCPServers, servers) {
		t.Fatalf("create MCP servers = %#v, want %#v", client.createRequest.MCPServers, servers)
	}
	if !reflect.DeepEqual(binder.boundLease, lease) {
		t.Fatalf("bound lease = %#v, want acquired turn lease", binder.boundLease)
	}
	wantContexts := []agentevent.Context{
		{},
		{
			AssignmentID: "assignment", AgentSessionID: "durable-session", ACPSessionID: "acp-new", TurnID: "turn",
			ExecutionEpoch: 3, ControlRevision: 7,
		},
	}
	if !reflect.DeepEqual(client.eventContexts, wantContexts) {
		t.Fatalf("Agent Event contexts = %#v, want %#v", client.eventContexts, wantContexts)
	}
	if got := string(binder.capabilities); got != `{"sessionCapabilities":{"list":{},"resume":{}},"z":true}` {
		t.Fatalf("bound capabilities = %s", got)
	}
}

func TestPrepareRecoversThenBindsBeforeContinuationAndConfiguration(t *testing.T) {
	operations := []string{}
	servers := []acp.MCPServer{{Name: "tools", Command: "/usr/local/bin/tools", Env: []acp.EnvironmentEntry{{Name: "SCOPE", Value: "review"}}}}
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	client := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"sessionCapabilities":{"list":{},"resume":{}}}`)},
		recovered:  acp.SessionInfo{ID: "acp-recovered", CWD: acp.WorkspacePath},
		continued:  configOptions("old/model", "low", "build"),
		configResponses: [][]json.RawMessage{
			configOptions("provider/model", "low", "build"),
			configOptions("provider/model", "low", "omnigrex-developer"),
		},
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	result, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease:         lease,
		Session:       durable,
		Client:        client,
		StatePath:     durable.RuntimeStatePath,
		MCPServers:    servers,
		Configuration: opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.ACPSessionID != "acp-recovered" || result.Disposition != session.Recovered {
		t.Fatalf("Prepare() result = %#v", result)
	}
	wantOperations := []string{
		"fence", "initialize", "fence", "recover:/workspace", "fence", "bind:acp-recovered",
		"fence", "continue:acp-recovered", "fence", "config:model", "fence", "config:mode",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
	if client.continueRequest.CWD != acp.WorkspacePath || !reflect.DeepEqual(client.continueRequest.MCPServers, servers) {
		t.Fatalf("continuation request = %#v", client.continueRequest)
	}
}

func TestPrepareKeepsRecoveredIdentityBoundWhenContinuationFails(t *testing.T) {
	operations := []string{}
	continuationErr := errors.New("continuation failed")
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	client := &agentClient{
		operations:  &operations,
		initialize:  acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"resume":true}`)},
		recovered:   acp.SessionInfo{ID: "acp-recovered", CWD: acp.WorkspacePath},
		continueErr: continuationErr,
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
	})
	if !errors.Is(err, continuationErr) {
		t.Fatalf("Prepare() error = %v, want continuation failure", err)
	}
	if binder.bound.ACPSessionID != "acp-recovered" || binder.bound.Status != store.AgentSessionActive {
		t.Fatalf("durable Session after continuation failure = %#v, want recovered ACTIVE binding", binder.bound)
	}
	if got := string(binder.bound.Capabilities); got != `{"resume":true}` {
		t.Fatalf("durable capabilities after continuation failure = %s", got)
	}
	wantOperations := []string{
		"fence", "initialize", "fence", "recover:/workspace", "fence", "bind:acp-recovered",
		"fence", "continue:acp-recovered",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
}

func TestPrepareRebindsActiveSessionBeforeContinuation(t *testing.T) {
	operations := []string{}
	capabilities := json.RawMessage(`{"sessionCapabilities":{"resume":{}},"feature":true}`)
	durable, lease := activeTurn(store.AgentSessionActive, "acp-existing", capabilities)
	client := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{ "feature": true, "sessionCapabilities": { "resume": {} } }`)},
		continued:  configOptions("old/model", "low", "build"),
		configResponses: [][]json.RawMessage{
			configOptions("provider/model", "low", "build"),
			configOptions("provider/model", "low", "omnigrex-developer"),
		},
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	result, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease:         lease,
		Session:       durable,
		Client:        client,
		StatePath:     durable.RuntimeStatePath,
		Configuration: opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.ACPSessionID != "acp-existing" || result.Disposition != session.Continued {
		t.Fatalf("Prepare() result = %#v", result)
	}
	wantContexts := []agentevent.Context{
		{},
		{
			AssignmentID: "assignment", AgentSessionID: "durable-session", ACPSessionID: "acp-existing", TurnID: "turn",
			ExecutionEpoch: 3, ControlRevision: 7,
		},
	}
	if !reflect.DeepEqual(client.eventContexts, wantContexts) {
		t.Fatalf("Agent Event contexts = %#v, want %#v", client.eventContexts, wantContexts)
	}
	wantOperations := []string{
		"fence", "initialize", "fence", "bind:acp-existing", "fence", "continue:acp-existing",
		"fence", "config:model", "fence", "config:mode",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
}

func TestPrepareDoesNotCreateWhenRecoveryIsAmbiguous(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	client := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{}`)},
		recoverErr: acp.ErrSessionAmbiguous,
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
		Configuration: opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"},
	})
	if !errors.Is(err, acp.ErrSessionAmbiguous) {
		t.Fatalf("Prepare() error = %v, want ErrSessionAmbiguous", err)
	}
	if want := []string{"fence", "initialize", "fence", "recover:/workspace"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestPrepareDoesNotCreateWhenRecoveryHasDiscoveryMismatch(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	client := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{}`)},
		recoverErr: acp.ErrSessionDiscoveryMismatch,
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
	})
	if !errors.Is(err, acp.ErrSessionDiscoveryMismatch) {
		t.Fatalf("Prepare() error = %v, want ErrSessionDiscoveryMismatch", err)
	}
	if want := []string{"fence", "initialize", "fence", "recover:/workspace"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestPrepareLaterTurnRecoversCreatingSessionAfterCreateResponseLoss(t *testing.T) {
	operations := []string{}
	responseLost := errors.New("session/new response lost")
	durable, firstLease := activeTurn(store.AgentSessionCreating, "", nil)
	binder := &bindingStore{operations: &operations, bound: durable}
	firstClient := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"sessionCapabilities":{"list":{},"resume":{}}}`)},
		recoverErr: acp.ErrSessionNotFound,
		createErr:  responseLost,
	}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: firstLease, Session: durable, Client: firstClient, StatePath: durable.RuntimeStatePath,
	})
	if !errors.Is(err, responseLost) {
		t.Fatalf("first Prepare() error = %v, want lost creation response", err)
	}
	if binder.bound.Status != store.AgentSessionCreating || binder.bound.ACPSessionID != "" {
		t.Fatalf("Session after lost creation response = %#v, want unchanged CREATING Session", binder.bound)
	}

	laterSession := binder.bound
	laterSession.NextExecutionEpoch++
	laterLease := laterTurnLease(firstLease)
	laterClient := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"sessionCapabilities":{"list":{},"resume":{}}}`)},
		recovered:  acp.SessionInfo{ID: "acp-created-before-loss", CWD: acp.WorkspacePath},
		continued:  configOptions("provider/model", "high", "omnigrex-developer"),
		configResponses: [][]json.RawMessage{
			configOptions("provider/model", "high", "omnigrex-developer"),
			configOptions("provider/model", "high", "omnigrex-developer"),
		},
	}
	result, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: laterLease, Session: laterSession, Client: laterClient, StatePath: laterSession.RuntimeStatePath,
		Configuration: opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"},
	})
	if err != nil {
		t.Fatalf("later Prepare() error = %v", err)
	}
	if result.Disposition != session.Recovered || result.ACPSessionID != "acp-created-before-loss" {
		t.Fatalf("later Prepare() result = %#v, want recovered Session", result)
	}
	if binder.bound.Status != store.AgentSessionActive || binder.bound.ACPSessionID != result.ACPSessionID {
		t.Fatalf("durable Session after recovery = %#v", binder.bound)
	}
	creates := 0
	for _, operation := range operations {
		if operation == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("session/new calls = %d, operations = %v, want only original call", creates, operations)
	}
}

func TestPrepareBindsIdentityBeforeConfigurationFailure(t *testing.T) {
	operations := []string{}
	configurationErr := errors.New("set option failed")
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	client := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"z":true,"a":[]}`)},
		recoverErr: acp.ErrSessionNotFound,
		created:    acp.Session{ID: "acp-bound", ConfigOptions: configOptions("old/model", "low", "build")},
		configErr:  configurationErr,
	}
	binder := &bindingStore{operations: &operations, bound: durable}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
		Configuration: opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"},
	})
	if !errors.Is(err, configurationErr) {
		t.Fatalf("Prepare() error = %v, want configuration failure", err)
	}
	wantOperations := []string{
		"fence", "initialize", "fence", "recover:/workspace", "fence", "create",
		"fence", "bind:acp-bound", "fence", "config:model",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
	if got := string(binder.capabilities); got != `{"a":[],"z":true}` {
		t.Fatalf("persisted capabilities = %s", got)
	}
}

func TestPrepareStopsBeforeACPWhenInitialTurnFenceIsStale(t *testing.T) {
	operations := []string{}
	stale := errors.New("stale turn")
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	binder := &bindingStore{operations: &operations, fenceErrors: []error{stale}}
	client := &agentClient{operations: &operations}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
	})
	if !errors.Is(err, stale) {
		t.Fatalf("Prepare() error = %v, want stale fence", err)
	}
	if want := []string{"fence"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestPrepareRevalidatesTurnFenceBeforeCreateAndConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		fenceErrors []error
		want        []string
	}{
		{
			name:        "before create",
			fenceErrors: []error{nil, nil, errors.New("stale before create")},
			want:        []string{"fence", "initialize", "fence", "recover:/workspace", "fence"},
		},
		{
			name:        "before first config option",
			fenceErrors: []error{nil, nil, nil, nil, errors.New("stale before config")},
			want: []string{
				"fence", "initialize", "fence", "recover:/workspace", "fence", "create",
				"fence", "bind:acp-new", "fence",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := []string{}
			durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
			binder := &bindingStore{operations: &operations, bound: durable, fenceErrors: test.fenceErrors}
			client := &agentClient{
				operations: &operations,
				initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{}`)},
				recoverErr: acp.ErrSessionNotFound,
				created:    acp.Session{ID: "acp-new", ConfigOptions: configOptions("old/model", "low", "build")},
			}

			_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
				Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
				Configuration: opencode.SessionConfiguration{Model: "provider/model", Mode: "omnigrex-developer"},
			})
			if err == nil || !reflect.DeepEqual(operations, test.want) {
				t.Fatalf("Prepare() = %v, operations = %v, want %v", err, operations, test.want)
			}
		})
	}
}

func TestPrepareRejectsCapabilityMismatchBeforeActiveBindOrContinuation(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionActive, "acp-existing", json.RawMessage(`{"version":1}`))
	client := &agentClient{
		operations: &operations,
		initialize: acp.InitializeResponse{RawAgentCapabilities: json.RawMessage(`{"version":2}`)},
	}
	binder := &bindingStore{operations: &operations}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
	})
	if !errors.Is(err, session.ErrCapabilityMismatch) {
		t.Fatalf("Prepare() error = %v, want ErrCapabilityMismatch", err)
	}
	if want := []string{"fence", "initialize"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestPrepareRejectsMismatchedTurnAndSessionIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.AgentSession, *store.AgentTurnLease)
	}{
		{name: "session", mutate: func(durable *store.AgentSession, _ *store.AgentTurnLease) { durable.ID = "other-session" }},
		{name: "assignment", mutate: func(durable *store.AgentSession, _ *store.AgentTurnLease) {
			durable.AgentAssignmentID = "other-assignment"
		}},
		{name: "control revision", mutate: func(durable *store.AgentSession, _ *store.AgentTurnLease) { durable.ControlRevision++ }},
		{name: "zero control revision", mutate: func(durable *store.AgentSession, lease *store.AgentTurnLease) {
			durable.ControlRevision, lease.ControlRevision = 0, 0
		}},
		{name: "job epoch", mutate: func(_ *store.AgentSession, lease *store.AgentTurnLease) { lease.JobLease.ExecutionEpoch++ }},
		{name: "durable next epoch", mutate: func(durable *store.AgentSession, _ *store.AgentTurnLease) { durable.NextExecutionEpoch++ }},
		{name: "zero epoch", mutate: func(durable *store.AgentSession, lease *store.AgentTurnLease) {
			durable.NextExecutionEpoch, lease.ExecutionEpoch = 1, 0
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := []string{}
			durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
			test.mutate(&durable, &lease)
			binder := &bindingStore{operations: &operations}
			client := &agentClient{operations: &operations}

			_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
				Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
			})
			if !errors.Is(err, session.ErrTurnIdentityMismatch) {
				t.Fatalf("Prepare() error = %v, want ErrTurnIdentityMismatch", err)
			}
			if len(operations) != 0 {
				t.Fatalf("operations = %v, want none", operations)
			}
		})
	}
}

func TestPrepareRejectsStatePathAndRetainedSessionBeforeRuntimeUse(t *testing.T) {
	tests := []struct {
		name      string
		statePath string
		status    store.AgentSessionStatus
		want      error
	}{
		{name: "empty path", status: store.AgentSessionCreating, want: session.ErrStatePathMismatch},
		{name: "different path", statePath: "/runtime/reviewer", status: store.AgentSessionCreating, want: session.ErrStatePathMismatch},
		{name: "retained", statePath: "/runtime/developer", status: store.AgentSessionRetained, want: session.ErrUnsupportedSessionState},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := []string{}
			durable, lease := activeTurn(test.status, "", nil)
			binder := &bindingStore{operations: &operations}
			client := &agentClient{operations: &operations}

			_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
				Lease: lease, Session: durable, Client: client, StatePath: test.statePath,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("Prepare() error = %v, want %v", err, test.want)
			}
			if len(operations) != 0 {
				t.Fatalf("operations = %v, want none", operations)
			}
		})
	}
}

func TestPrepareRejectsAdditionalDirectories(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionCreating, "", nil)
	binder := &bindingStore{operations: &operations}
	client := &agentClient{operations: &operations}

	_, err := session.NewCoordinator(binder).Prepare(context.Background(), session.PrepareRequest{
		Lease: lease, Session: durable, Client: client, StatePath: durable.RuntimeStatePath,
		AdditionalDirectories: []string{"/context"},
	})
	if !errors.Is(err, session.ErrAdditionalDirectoriesUnsupported) {
		t.Fatalf("Prepare() error = %v, want ErrAdditionalDirectoriesUnsupported", err)
	}
	if len(operations) != 0 {
		t.Fatalf("operations = %v, want none", operations)
	}
}

func TestPromptSubmitsOnlyAfterValidatingCurrentTurnFence(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionActive, "acp-existing", json.RawMessage(`{}`))
	binder := &bindingStore{operations: &operations}
	client := &agentClient{
		operations:     &operations,
		promptResponse: acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
	}
	content := []acp.ContentBlock{acp.TextContent("Continue the Work Item.")}

	response, err := session.NewCoordinator(binder).Prompt(context.Background(), session.PromptRequest{
		Lease: lease, Session: durable, Client: client, Content: content,
	})
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if response.StopReason != acp.StopReasonEndTurn || !reflect.DeepEqual(client.promptContent, content) {
		t.Fatalf("Prompt() = (%#v, %#v), want submitted content", response, client.promptContent)
	}
	if want := (agentevent.Context{
		AssignmentID: "assignment", AgentSessionID: "durable-session", ACPSessionID: "acp-existing", TurnID: "turn",
		ExecutionEpoch: 3, ControlRevision: 7,
	}); !reflect.DeepEqual(client.eventContext, want) {
		t.Fatalf("Agent Event context = %#v, want durable context %#v", client.eventContext, want)
	}
	if want := []string{"prompt-fence:acp-existing", "prompt:acp-existing"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestPromptRejectsStaleControlRevisionBeforeSubmission(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionActive, "acp-existing", json.RawMessage(`{}`))
	stale := errors.New("stale control revision")
	binder := &bindingStore{operations: &operations, promptFenceErrors: []error{stale}}
	client := &agentClient{operations: &operations}

	_, err := session.NewCoordinator(binder).Prompt(context.Background(), session.PromptRequest{
		Lease: lease, Session: durable, Client: client, Content: []acp.ContentBlock{acp.TextContent("Do not submit")},
	})
	if !errors.Is(err, stale) {
		t.Fatalf("Prompt() error = %v, want stale control revision", err)
	}
	if want := []string{"prompt-fence:acp-existing"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestPromptRejectsPersistedACPSessionMismatchBeforeSubmission(t *testing.T) {
	operations := []string{}
	durable, lease := activeTurn(store.AgentSessionActive, "acp-stale-snapshot", json.RawMessage(`{}`))
	binder := &bindingStore{operations: &operations, persistedACPSessionID: "acp-current"}
	client := &agentClient{operations: &operations}

	_, err := session.NewCoordinator(binder).Prompt(context.Background(), session.PromptRequest{
		Lease: lease, Session: durable, Client: client, Content: []acp.ContentBlock{acp.TextContent("Do not submit")},
	})
	if !errors.Is(err, store.ErrAgentSessionACPConflict) {
		t.Fatalf("Prompt() error = %v, want ErrAgentSessionACPConflict", err)
	}
	if want := []string{"prompt-fence:acp-stale-snapshot"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestHumanPromptReplaysHistoryAndSubmitsOnlyUnderCurrentHumanFence(t *testing.T) {
	operations := []string{}
	durable, _ := activeTurn(store.AgentSessionActive, "acp-existing", json.RawMessage(`{}`))
	durable.ControlOwner = store.SessionControlHuman
	binder := &bindingStore{operations: &operations}
	client := &agentClient{
		operations:     &operations,
		promptResponse: acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
	}
	content := []acp.ContentBlock{acp.TextContent("human follow-up")}

	response, err := session.NewCoordinator(binder).HumanPrompt(context.Background(), session.HumanPromptRequest{
		Session: durable, Client: client, Content: content,
	})
	if err != nil {
		t.Fatalf("HumanPrompt() error = %v", err)
	}
	if response.StopReason != acp.StopReasonEndTurn || !reflect.DeepEqual(client.promptContent, content) {
		t.Fatalf("HumanPrompt() = (%#v, %#v), want submitted content", response, client.promptContent)
	}
	want := []string{
		"human-prompt-acquire:acp-existing", "replay:acp-existing",
		"human-prompt-heartbeat:acp-existing", "prompt:acp-existing", "human-prompt-release:acp-existing",
	}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestHumanPromptRejectsFenceChangedDuringReplayBeforeSubmission(t *testing.T) {
	tests := []struct {
		name string
		want error
	}{
		{name: "stale control revision", want: store.ErrAgentSessionControlFenceLost},
		{name: "changed ACP identity", want: store.ErrAgentSessionACPConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := []string{}
			durable, _ := activeTurn(store.AgentSessionActive, "acp-existing", json.RawMessage(`{}`))
			durable.ControlOwner = store.SessionControlHuman
			binder := &bindingStore{operations: &operations, humanFenceErrors: []error{nil, test.want}}
			client := &agentClient{operations: &operations}

			_, err := session.NewCoordinator(binder).HumanPrompt(context.Background(), session.HumanPromptRequest{
				Session: durable, Client: client, Content: []acp.ContentBlock{acp.TextContent("must not submit")},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("HumanPrompt() error = %v, want %v", err, test.want)
			}
			wantOperations := []string{
				"human-prompt-acquire:acp-existing", "replay:acp-existing",
				"human-prompt-heartbeat:acp-existing", "human-prompt-release:acp-existing",
			}
			if !reflect.DeepEqual(operations, wantOperations) {
				t.Fatalf("operations = %v, want no prompt after replay: %v", operations, wantOperations)
			}
		})
	}
}

func TestHumanPromptReportsUncertainOutcomeWhenExactReleaseFailsAfterSuccess(t *testing.T) {
	operations := []string{}
	durable, _ := activeTurn(store.AgentSessionActive, "acp-existing", json.RawMessage(`{}`))
	durable.ControlOwner = store.SessionControlHuman
	binder := &bindingStore{operations: &operations, humanReleaseErr: store.ErrHumanPromptLeaseLost}
	client := &agentClient{
		operations:     &operations,
		promptResponse: acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
	}

	response, err := session.NewCoordinator(binder).HumanPrompt(context.Background(), session.HumanPromptRequest{
		Session: durable, Client: client, Content: []acp.ContentBlock{acp.TextContent("human follow-up")},
	})
	if !errors.Is(err, session.ErrHumanPromptOutcomeUncertain) || !errors.Is(err, store.ErrHumanPromptLeaseLost) {
		t.Fatalf("HumanPrompt() error = %v, want uncertain outcome and lease loss", err)
	}
	if response != (acp.PromptResponse{}) {
		t.Fatalf("HumanPrompt() response = %#v, want no successful response", response)
	}
}

func activeTurn(status store.AgentSessionStatus, acpSessionID string, capabilities json.RawMessage) (store.AgentSession, store.AgentTurnLease) {
	durable := store.AgentSession{
		ID: "durable-session", AgentAssignmentID: "assignment", ACPSessionID: acpSessionID,
		RuntimeStatePath: "/runtime/developer", Capabilities: capabilities, Status: status,
		ControlRevision: 7, NextExecutionEpoch: 4,
	}
	lease := store.AgentTurnLease{
		AgentTurn: store.AgentTurn{
			AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: durable.ID, ControlRevision: durable.ControlRevision},
			ID:            "turn", AgentAssignmentID: durable.AgentAssignmentID, ExecutionEpoch: 3,
		},
		JobLease: store.JobLease{Job: store.Job{
			ID: "job", AgentAssignmentID: durable.AgentAssignmentID, AgentSessionID: durable.ID,
			AgentTurnID: "turn", ExecutionEpoch: 3,
		}, Attempt: 1},
		OwnerID: "runtime", OwnerToken: "owner-token",
	}
	return durable, lease
}

func laterTurnLease(first store.AgentTurnLease) store.AgentTurnLease {
	later := first
	later.ID = "later-turn"
	later.ExecutionEpoch++
	later.JobLease.ID = "later-job"
	later.JobLease.AgentTurnID = later.ID
	later.JobLease.ExecutionEpoch = later.ExecutionEpoch
	return later
}

type bindingStore struct {
	operations            *[]string
	bound                 store.AgentSession
	boundLease            store.AgentTurnLease
	capabilities          json.RawMessage
	fenceErrors           []error
	promptFenceErrors     []error
	humanFenceErrors      []error
	humanReleaseErr       error
	persistedACPSessionID string
	bindErr               error
}

func (binder *bindingStore) ValidateTurnFence(_ context.Context, _ store.AgentTurnLease) error {
	*binder.operations = append(*binder.operations, "fence")
	if len(binder.fenceErrors) == 0 {
		return nil
	}
	err := binder.fenceErrors[0]
	binder.fenceErrors = binder.fenceErrors[1:]
	return err
}

func (binder *bindingStore) ValidateAgentSessionPromptFence(_ context.Context, _ store.AgentTurnLease, sessionID string) error {
	*binder.operations = append(*binder.operations, "prompt-fence:"+sessionID)
	if len(binder.promptFenceErrors) != 0 {
		err := binder.promptFenceErrors[0]
		binder.promptFenceErrors = binder.promptFenceErrors[1:]
		return err
	}
	if binder.persistedACPSessionID != "" && binder.persistedACPSessionID != sessionID {
		return store.ErrAgentSessionACPConflict
	}
	return nil
}

func (binder *bindingStore) AcquireHumanPromptLease(_ context.Context, sessionID string, revision int64, acpSessionID string, _ time.Duration) (store.HumanPromptLease, error) {
	*binder.operations = append(*binder.operations, "human-prompt-acquire:"+acpSessionID)
	if len(binder.humanFenceErrors) == 0 {
		return store.HumanPromptLease{AgentSessionID: sessionID, ControlRevision: revision, ACPSessionID: acpSessionID, Token: "lease-token"}, nil
	}
	err := binder.humanFenceErrors[0]
	binder.humanFenceErrors = binder.humanFenceErrors[1:]
	if err != nil {
		return store.HumanPromptLease{}, err
	}
	return store.HumanPromptLease{AgentSessionID: sessionID, ControlRevision: revision, ACPSessionID: acpSessionID, Token: "lease-token"}, nil
}

func (binder *bindingStore) HeartbeatHumanPromptLease(_ context.Context, lease store.HumanPromptLease, _ time.Duration) error {
	*binder.operations = append(*binder.operations, "human-prompt-heartbeat:"+lease.ACPSessionID)
	if len(binder.humanFenceErrors) == 0 {
		return nil
	}
	err := binder.humanFenceErrors[0]
	binder.humanFenceErrors = binder.humanFenceErrors[1:]
	return err
}

func (binder *bindingStore) ReleaseHumanPromptLease(_ context.Context, lease store.HumanPromptLease) error {
	*binder.operations = append(*binder.operations, "human-prompt-release:"+lease.ACPSessionID)
	return binder.humanReleaseErr
}

func (binder *bindingStore) BindAgentSessionACP(_ context.Context, lease store.AgentTurnLease, sessionID string, capabilities json.RawMessage) (store.AgentSession, error) {
	*binder.operations = append(*binder.operations, "bind:"+sessionID)
	binder.boundLease = lease
	binder.capabilities = append(json.RawMessage(nil), capabilities...)
	if binder.bindErr != nil {
		return store.AgentSession{}, binder.bindErr
	}
	bound := binder.bound
	bound.ACPSessionID = sessionID
	bound.Capabilities = append(json.RawMessage(nil), capabilities...)
	bound.Status = store.AgentSessionActive
	binder.bound = bound
	return bound, nil
}

type agentClient struct {
	operations          *[]string
	initialize          acp.InitializeResponse
	initializeErr       error
	recovered           acp.SessionInfo
	recoverErr          error
	created             acp.Session
	createErr           error
	continued           []json.RawMessage
	continueErr         error
	configResponses     [][]json.RawMessage
	configErr           error
	createRequest       acp.CreateSessionRequest
	continueRequest     acp.ContinueSessionRequest
	promptResponse      acp.PromptResponse
	promptErr           error
	promptContent       []acp.ContentBlock
	eventContext        agentevent.Context
	eventContexts       []agentevent.Context
	recordEventContexts bool
}

func (client *agentClient) SetAgentEventContext(eventContext agentevent.Context) {
	client.eventContext = eventContext
	client.eventContexts = append(client.eventContexts, eventContext)
	if client.recordEventContexts {
		*client.operations = append(*client.operations, "event-context:"+eventContext.ACPSessionID)
	}
}

func (client *agentClient) Prompt(_ context.Context, sessionID string, content []acp.ContentBlock) (acp.PromptResponse, error) {
	*client.operations = append(*client.operations, "prompt:"+sessionID)
	client.promptContent = append([]acp.ContentBlock(nil), content...)
	return client.promptResponse, client.promptErr
}

func (client *agentClient) ReplayHistory(_ context.Context, request acp.ContinueSessionRequest) error {
	*client.operations = append(*client.operations, "replay:"+request.SessionID)
	client.continueRequest = request
	return client.continueErr
}

func (client *agentClient) Initialize(context.Context) (acp.InitializeResponse, error) {
	*client.operations = append(*client.operations, "initialize")
	return client.initialize, client.initializeErr
}

func (client *agentClient) RecoverCreatedSession(_ context.Context, cwd string) (acp.SessionInfo, error) {
	*client.operations = append(*client.operations, "recover:"+cwd)
	return client.recovered, client.recoverErr
}

func (client *agentClient) CreateSession(_ context.Context, request acp.CreateSessionRequest) (acp.Session, error) {
	*client.operations = append(*client.operations, "create")
	client.createRequest = request
	return client.created, client.createErr
}

func (client *agentClient) ContinueSessionWithOptions(_ context.Context, request acp.ContinueSessionRequest) ([]json.RawMessage, error) {
	*client.operations = append(*client.operations, "continue:"+request.SessionID)
	client.continueRequest = request
	return client.continued, client.continueErr
}

func (client *agentClient) SetConfigOption(_ context.Context, _ string, configID string, _ any) ([]json.RawMessage, error) {
	*client.operations = append(*client.operations, "config:"+configID)
	if client.configErr != nil {
		return nil, client.configErr
	}
	response := client.configResponses[0]
	client.configResponses = client.configResponses[1:]
	return response, nil
}

func configOptions(model, effort, mode string) []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"id":"model","type":"select","currentValue":"` + model + `","options":[{"value":"old/model"},{"value":"provider/model"}]}`),
		json.RawMessage(`{"id":"effort","type":"select","currentValue":"` + effort + `","options":[{"value":"low"},{"value":"high"}]}`),
		json.RawMessage(`{"id":"mode","type":"select","currentValue":"` + mode + `","options":[{"value":"build"},{"value":"omnigrex-developer"}]}`),
	}
}
