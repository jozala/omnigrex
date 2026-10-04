//go:build integration

package acp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/opencode"
)

const (
	repositorySkillName        = "checkout-compatibility"
	repositorySkillDescription = "REPOSITORY_SKILL_DESCRIPTION"
	repositorySkillBody        = "REPOSITORY_SKILL_BODY_INITIAL"
	repositorySkillNextBody    = "REPOSITORY_SKILL_BODY_UPDATED"
	repositorySkillPrompt      = "LOAD_CHECKOUT_SKILL"
	skillDisabledPrompt        = "VERIFY_SKILL_DISABLED"
	skillDisabledMarker        = "SKILL_DISABLED_CONFIRMED"
	skillMissingPrompt         = "LOAD_MISSING_CHECKOUT_SKILL"
	skillMissingMarker         = "SKILL_NOT_FOUND_CONFIRMED"
)

func TestOpenCodeRepositorySkillsFollowCheckoutAcrossTurns(t *testing.T) {
	for _, roleID := range []opencode.Role{opencode.RoleDeveloper, opencode.RoleReviewer} {
		t.Run(string(roleID), func(t *testing.T) {
			image := localOpenCodeImage(t)
			provider := startFakeProvider(t, http.HandlerFunc(handleSkillProvider))
			rendered := renderSkillProfile(t, roleID, true)
			workspaceVolume, stateVolume, miseVolume := skillVolumes(t)
			if roleID == opencode.RoleReviewer {
				mcp := startTestMCPServer(t)
				writeReviewerIsolationFixture(t, workspaceVolume, mcp.URL)
				defer mcp.assertNoRequests(t)
			} else {
				writeRepositorySkill(t, workspaceVolume, repositorySkillDocument(repositorySkillBody))
			}

			updates := make(chan acp.SessionUpdate, 128)
			first := startOpenCodeWithTransport(t, image, workspaceVolume, stateVolume, miseVolume, provider, updates, nil,
				openCodeTestOptions{rendered: rendered})
			initializeOpenCode(t, first.client)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			session, err := first.client.CreateSession(ctx, acp.CreateSessionRequest{CWD: acp.WorkspacePath})
			cancel()
			if err != nil {
				t.Fatalf("CreateSession: %v\n%s", err, first.stderr.String())
			}
			assertConfigOptionHasValue(t, session.ConfigOptions, "mode", rendered.SessionConfiguration().Mode)
			prompt(t, first, session.ID, repositorySkillPrompt)
			waitForAgentText(t, updates, repositorySkillBody)
			assertSkillPermissionRequest(t, first)
			if roleID == opencode.RoleReviewer {
				prompt(t, first, session.ID, reviewerPrompt)
				waitForAgentText(t, updates, reviewerMarker)
			}
			first.stop(t)

			// Only the checkout changes; the retained Agent Session still contains the old skill.
			writeRepositorySkill(t, workspaceVolume, repositorySkillDocument(repositorySkillNextBody))
			second := startOpenCodeWithTransport(t, image, workspaceVolume, stateVolume, miseVolume, provider, updates, nil,
				openCodeTestOptions{rendered: rendered})
			initializeOpenCode(t, second.client)
			ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
			err = second.client.ContinueSession(ctx, acp.ContinueSessionRequest{SessionID: session.ID, CWD: acp.WorkspacePath})
			cancel()
			if err != nil {
				t.Fatalf("ContinueSession: %v\n%s", err, second.stderr.String())
			}
			prompt(t, second, session.ID, repositorySkillPrompt)
			waitForAgentText(t, updates, repositorySkillNextBody)
			assertSkillPermissionRequest(t, second)
			second.stop(t)
		})
	}
}

func TestOpenCodeRepositorySkillsUnavailableAndDenied(t *testing.T) {
	for _, test := range []struct {
		name     string
		role     opencode.Role
		enabled  bool
		document string
		reject   bool
		prompt   string
		want     string
	}{
		{"developer permission omitted", opencode.RoleDeveloper, false, repositorySkillDocument(repositorySkillBody), false, skillDisabledPrompt, skillDisabledMarker},
		{"reviewer permission omitted", opencode.RoleReviewer, false, repositorySkillDocument(repositorySkillBody), false, skillDisabledPrompt, skillDisabledMarker},
		{"missing directory", opencode.RoleReviewer, true, "", false, skillMissingPrompt, skillMissingMarker},
		{"invalid front matter", opencode.RoleReviewer, true, "---\nname: [invalid]\ndescription: " + repositorySkillDescription + "\n---\n" + repositorySkillBody, false, skillMissingPrompt, skillMissingMarker},
		{"ACP rejects loading", opencode.RoleReviewer, true, repositorySkillDocument(repositorySkillBody), true, repositorySkillPrompt, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := localOpenCodeImage(t)
			provider := startFakeProvider(t, http.HandlerFunc(handleSkillProvider))
			workspaceVolume, stateVolume, miseVolume := skillVolumes(t)
			if test.document != "" {
				writeRepositorySkill(t, workspaceVolume, test.document)
			}
			options := openCodeTestOptions{rendered: renderSkillProfile(t, test.role, test.enabled)}
			if test.reject {
				denied := renderSkillProfile(t, test.role, false)
				options.decidePermission = denied.DecidePermission
			}
			updates := make(chan acp.SessionUpdate, 128)
			process := startOpenCodeWithTransport(t, image, workspaceVolume, stateVolume, miseVolume, provider, updates, nil, options)
			initializeOpenCode(t, process.client)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			session, err := process.client.CreateSession(ctx, acp.CreateSessionRequest{CWD: acp.WorkspacePath})
			cancel()
			if err != nil {
				t.Fatalf("CreateSession: %v\n%s", err, process.stderr.String())
			}
			prompt(t, process, session.ID, test.prompt)
			if test.reject {
				assertSkillPermissionRequest(t, process)
				assertSkillLoadFailed(t, updates)
			} else {
				waitForAgentText(t, updates, test.want)
			}
			process.stop(t)
		})
	}
}

func renderSkillProfile(t *testing.T, roleID opencode.Role, enabled bool) *opencode.RenderedProfile {
	t.Helper()
	permissions := opencode.PermissionPolicy{"read": opencode.PermissionAllow}
	if enabled {
		permissions["skill"] = opencode.PermissionAllow
	}
	rendered, err := opencode.Render(roleID, opencode.Profile{
		Instructions: roleInstruction, Model: "fake/fake-model", Steps: 10, Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

// Use the production Role renderer while retaining only the test provider's connection configuration.
func providerProfileEnvironment(t *testing.T, provider string, rendered *opencode.RenderedProfile) []string {
	t.Helper()
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(provider), &config); err != nil {
		t.Fatal(err)
	}
	environment := rendered.Environment()
	for index, entry := range environment {
		value, found := strings.CutPrefix(entry, "OPENCODE_CONFIG_CONTENT=")
		if !found {
			continue
		}
		var roleConfig map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &roleConfig); err != nil {
			t.Fatal(err)
		}
		for key, value := range roleConfig {
			config[key] = value
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		environment[index] = "OPENCODE_CONFIG_CONTENT=" + string(encoded)
		return environment
	}
	t.Fatal("rendered Profile has no OpenCode configuration")
	return nil
}

func handleSkillProvider(response http.ResponseWriter, request *http.Request) {
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
	active := latestUserMessage(chat)
	if active != repositorySkillPrompt && active != skillDisabledPrompt && active != skillMissingPrompt {
		request.Body = io.NopCloser(bytes.NewReader(body))
		handleFakeProvider(response, request)
		return
	}
	if hasFakeTool(chat, "task") || hasFakeTool(chat, "edit") || hasFakeTool(chat, "omnigrex_publish_changes") {
		writeFakeText(response, "SKILL_EXPANDED_PERMISSIONS")
		return
	}
	if active == skillDisabledPrompt {
		if hasFakeTool(chat, "skill") || bytes.Contains(body, []byte(repositorySkillDescription)) {
			writeFakeText(response, "DISABLED_SKILL_EXPOSED")
		} else {
			writeFakeText(response, skillDisabledMarker)
		}
		return
	}
	if output := latestToolMessage(chat); output != "" {
		switch {
		case active == skillMissingPrompt && strings.Contains(output, "not found") && !strings.Contains(output, repositorySkillBody):
			writeFakeText(response, skillMissingMarker)
		default:
			writeFakeText(response, output)
		}
		return
	}
	if !hasFakeTool(chat, "skill") || active != skillMissingPrompt && !bytes.Contains(body, []byte(repositorySkillDescription)) {
		writeFakeText(response, "SKILL_DISCOVERY_MISSING")
		return
	}
	// Descriptions are advertised; bodies must not be injected into the system prompt eagerly.
	for _, message := range chat.Messages {
		if message.Role == "system" && (bytes.Contains(message.Content, []byte(repositorySkillBody)) || bytes.Contains(message.Content, []byte(repositorySkillNextBody))) {
			writeFakeText(response, "SKILL_BODY_LOADED_EAGERLY")
			return
		}
	}
	writeFakeToolCall(response, "skill", map[string]any{"name": repositorySkillName})
}

func assertSkillPermissionRequest(t *testing.T, process *openCodeProcess) {
	t.Helper()
	select {
	case request := <-process.permissionRequests:
		if !bytes.Contains(request.ToolCall, []byte("skill")) {
			t.Fatalf("unexpected permission request: %s", request.ToolCall)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("skill loading did not request ACP permission")
	}
}

func assertSkillLoadFailed(t *testing.T, updates <-chan acp.SessionUpdate) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var observed []string
	for {
		select {
		case update := <-updates:
			observed = append(observed, string(update.Update))
			if bytes.Contains(update.Update, []byte(repositorySkillBody)) {
				t.Fatalf("rejected skill exposed its body: %s", update.Update)
			}
			var event struct {
				Kind   string `json:"sessionUpdate"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(update.Update, &event); err != nil {
				t.Fatal(err)
			}
			if event.Kind == "tool_call_update" && event.Status == "failed" {
				return
			}
		case <-timer.C:
			t.Fatalf("rejected skill did not emit a failed tool event: %s", strings.Join(observed, "\n"))
		}
	}
}

func skillVolumes(t *testing.T) (string, string, string) {
	t.Helper()
	workspaceVolume := uniqueDockerName("skills-workspace")
	stateVolume := uniqueDockerName("skills-state")
	miseVolume := uniqueDockerName("skills-mise")
	for _, volume := range []string{workspaceVolume, stateVolume, miseVolume} {
		createDockerVolume(t, volume)
	}
	return workspaceVolume, stateVolume, miseVolume
}

func repositorySkillDocument(body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s\n", repositorySkillName, repositorySkillDescription, body)
}

func writeRepositorySkill(t *testing.T, volume, document string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--mount", "type=volume,src="+volume+",dst=/volume",
		"alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
		"sh", "-ec", `mkdir -p "$1" && printf '%s' "$2" > "$1/SKILL.md" && chown -R 10001:10001 /volume/assignment`,
		"sh", "/volume/assignment/.agents/skills/"+repositorySkillName, document)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("write repository skill: %v\n%s", err, output)
	}
}
