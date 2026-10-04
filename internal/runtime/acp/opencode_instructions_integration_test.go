//go:build integration

package acp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentinstructions"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestOpenCodeContinuedSessionUsesCurrentDeploymentInstructions(t *testing.T) {
	image := localOpenCodeImage(t)
	provider := startFakeProvider(t, http.HandlerFunc(handleDeploymentInstructionProvider))
	workspaceVolume, stateVolume, miseVolume := skillVolumes(t)
	directory := t.TempDir()
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		t.Fatal(err)
	}
	stage, _ := definition.Stage(workflow.StageReview)
	policies := role.BuiltinPolicyCatalog()
	policy, _ := policies.Lookup(role.Reviewer)
	var sessionID string
	for _, generation := range []string{"DEPLOYMENT_V1", "DEPLOYMENT_V2"} {
		if err := os.WriteFile(filepath.Join(directory, "common.md"), []byte(generation), 0o600); err != nil {
			t.Fatal(err)
		}
		operator, err := agentinstructions.Load(directory, policies)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := opencode.Render(opencode.RoleReviewer, opencode.Profile{
			Instructions: operator.Compose(policy, stage, workflow.TurnPurposeReview, "STABLE_REPOSITORY_PERSONALITY"),
			Model:        "fake/fake-model", Steps: 10, Permissions: opencode.PermissionPolicy{"read": opencode.PermissionAllow},
		})
		if err != nil {
			t.Fatal(err)
		}
		updates := make(chan acp.SessionUpdate, 128)
		process := startOpenCodeWithTransport(t, image, workspaceVolume, stateVolume, miseVolume, provider, updates, nil, openCodeTestOptions{rendered: rendered})
		initializeOpenCode(t, process.client)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if sessionID == "" {
			session, createErr := process.client.CreateSession(ctx, acp.CreateSessionRequest{CWD: acp.WorkspacePath})
			err, sessionID = createErr, session.ID
		} else {
			err = process.client.ContinueSession(ctx, acp.ContinueSessionRequest{SessionID: sessionID, CWD: acp.WorkspacePath})
		}
		cancel()
		if err != nil {
			t.Fatalf("prepare %s Session: %v", generation, err)
		}
		prompt(t, process, sessionID, "VERIFY_"+generation)
		waitForAgentText(t, updates, "CURRENT_"+generation)
		process.stop(t)
	}
}

func handleDeploymentInstructionProvider(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	var chat fakeChatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	prompt := latestUserMessage(chat)
	if prompt != "VERIFY_DEPLOYMENT_V1" && prompt != "VERIFY_DEPLOYMENT_V2" {
		request.Body = io.NopCloser(bytes.NewReader(body))
		handleFakeProvider(response, request)
		return
	}
	var system strings.Builder
	for _, message := range chat.Messages {
		if message.Role == "system" {
			system.WriteString(messageText(message.Content))
		}
	}
	want := strings.TrimPrefix(prompt, "VERIFY_")
	other := "DEPLOYMENT_V1"
	if want == other {
		other = "DEPLOYMENT_V2"
	}
	text := system.String()
	if strings.Contains(text, want) && !strings.Contains(text, other) &&
		strings.Contains(text, "STABLE_REPOSITORY_PERSONALITY") && strings.Contains(text, "submit_review.comments") &&
		strings.Contains(text, "Omnigrex platform instructions") {
		writeFakeText(response, "CURRENT_"+want)
		return
	}
	writeFakeText(response, "DEPLOYMENT_INSTRUCTIONS_STALE_OR_MISSING")
}
