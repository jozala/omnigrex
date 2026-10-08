package agentturn_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestRepositoryImageSelectionIsPerParticipantAndDefaultBranchProvenanceIsSeparate(t *testing.T) {
	source := validProfileSource()
	imageA := "registry.example/repository@sha256:" + strings.Repeat("b", 64)
	imageB := "registry.example/repository@sha256:" + strings.Repeat("c", 64)
	source.contents[".omnigrex/environment.yaml"] = []byte("version: 1\nimage: " + imageA + "\nplatform: linux/amd64\nrequirements: [tool-installation]\n")
	base := runtimeProfile(t, testImage)
	registry, _ := profile.NewRegistry(base)
	database := &preparationStore{selected: workflow.RoleDeveloper}
	preparer := agentturn.NewPreparer(agentprofile.NewLoader(source, role.BuiltinPolicyCatalog()), agentprofile.SingletonSelector{}, registry, database)
	first, err := preparer.Prepare(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if first.RuntimeProfile.Contract().Image != imageA || len(database.calls[0].Developer.RuntimeContract) == 0 {
		t.Fatal("repository image contract was not selected and persisted")
	}
	for _, call := range source.fetchCalls {
		if call.commitSHA != source.commitSHA {
			t.Fatal("image or Profile was read from a different revision")
		}
	}
	source.contents[".omnigrex/environment.yaml"] = []byte("version: 1\nimage: " + imageB + "\nplatform: linux/arm64\n")
	catalog, _ := profile.NewCatalog([]profile.Profile{base}, []profile.Profile{first.RuntimeProfile})
	database = &preparationStore{selected: workflow.RoleDeveloper, bindings: store.AgentTurnPreparationRuntimeBindings{Participant: &first.Commit.Assignment.AssignmentRuntimeBinding}}
	preparer = agentturn.NewPreparer(agentprofile.NewLoader(source, role.BuiltinPolicyCatalog()), agentprofile.SingletonSelector{}, catalog, database)
	retained, err := preparer.Prepare(context.Background(), validRequest())
	if err != nil || retained.RuntimeProfile.Binding() != first.RuntimeProfile.Binding() {
		t.Fatalf("retained Participant reselected image: %+v %v", retained.RuntimeProfile.Contract(), err)
	}
	database = &preparationStore{selected: workflow.RoleReviewer}
	preparer = agentturn.NewPreparer(agentprofile.NewLoader(source, role.BuiltinPolicyCatalog()), agentprofile.SingletonSelector{}, catalog, database)
	reviewer, err := preparer.Prepare(context.Background(), validRequest())
	if err != nil || reviewer.RuntimeProfile.Contract().Image != imageB || reviewer.RuntimeProfile.Contract().Platform.Arch != "arm64" {
		t.Fatalf("later Reviewer did not select current environment: %+v %v", reviewer.RuntimeProfile.Contract(), err)
	}
}

func TestUnsupportedDockerCapabilitiesFailBeforeRepositoryWork(t *testing.T) {
	for _, requirement := range []string{"hardware-isolation", "nested-containers"} {
		t.Run(requirement, func(t *testing.T) {
			operations := []string{}
			base := runtimeLauncherProfile(t)
			bound, err := base.WithEnvironment(base.Contract().Image, base.Contract().Platform, []string{requirement})
			if err != nil {
				t.Fatal(err)
			}
			state, lease := runtimeExecutionContext(t, bound, workflow.RoleDeveloper, nil)
			launcher := runtimeLauncher(t, agentturn.LauncherConfig{
				Store: &runtimeStore{operations: &operations, execution: state}, Registry: &runtimeRegistry{operations: &operations, runtimeProfile: bound},
				Workspace: &runtimeWorkspace{operations: &operations}, Gateway: &runtimeGateway{operations: &operations},
				Execution: &runtimeEngineFactory{operations: &operations}, ACP: &runtimeACPFactory{operations: &operations}, Sessions: &runtimeSessionPreparer{operations: &operations},
			})
			handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git", DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "feature", RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"key":"test"}}`)})
			if handle != nil || !errors.Is(err, execution.ErrUnsupportedRequirement) || !strings.Contains(err.Error(), requirement) {
				t.Fatalf("unsupported capability: %v %v", handle, err)
			}
			if slices.Contains(operations, "workspace") || slices.Contains(operations, "mcp-register") || slices.Contains(operations, "docker-new") {
				t.Fatalf("unsupported environment began repository work: %v", operations)
			}
		})
	}
}

func TestRetainedPreStep07ParticipantProvisionsToolsWithoutRebinding(t *testing.T) {
	current := runtimeLauncherProfile(t)
	contract := current.Contract()
	contract.RepositoryEnvironment = nil
	contract.Image = "registry.example/retained@sha256:" + strings.Repeat("b", 64)
	legacy, err := profile.New(contract)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the historical binding through the deployment's current catalog,
	// just as the first Turn following an orchestrator upgrade does.
	catalog, err := profile.NewCatalog([]profile.Profile{current}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, lease := runtimeExecutionContext(t, legacy, workflow.RoleDeveloper, nil)
	state.Assignment.RuntimeImageDigest = contract.Image
	state.Session.RuntimeImageDigest = contract.Image
	state.Session.Status = store.AgentSessionActive
	state.Session.ACPSessionID = "retained-acp-session"
	operations := []string{}
	factory := &runtimeEngineFactory{operations: &operations}
	workspaces := &runtimeWorkspace{operations: &operations}
	sessions := &runtimeSessionPreparer{operations: &operations, result: session.Result{AgentSessionID: state.Session.ID, ACPSessionID: state.Session.ACPSessionID}}
	launcher := runtimeLauncher(t, agentturn.LauncherConfig{
		Store: &runtimeStore{operations: &operations, execution: state}, Registry: catalog,
		Workspace: workspaces, Gateway: &runtimeGateway{operations: &operations},
		Execution: factory, ACP: &runtimeACPFactory{operations: &operations, client: &runtimeACPClient{operations: &operations}}, Sessions: sessions,
		LegacyBootstrapImage: current.Contract().Image,
	})
	handle, err := launcher.Launch(context.Background(), agentturn.LaunchRequest{
		Lease: lease, LeaseDuration: time.Minute, RepositoryURL: "https://github.example/acme/widgets.git",
		DefaultBranchName: "trunk", DefaultBranchSHA: runtimeTestDefaultSHA, InitialFeatureBranch: "feature",
		RepositoryCredential: runtimeTestCredential, ProviderCredentialJSON: json.RawMessage(`{"openai":{"key":"test"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Cleanup(context.Background()) })
	if !slices.Equal(factory.engine.spec.Command, []string{"/opt/omnigrex/environment-bootstrap/omnigrex-environment", "run", "acp"}) {
		t.Fatal("retained Participant bypassed in-environment tool provisioning and activation")
	}
	if factory.request.BootstrapImage != current.Contract().Image {
		t.Fatal("retained provisioning did not select the exact deployment bootstrap artifact")
	}
	bootstrapMounted := false
	for _, mount := range factory.engine.spec.Volumes {
		if mount.Target == "/opt/omnigrex/environment-bootstrap" {
			bootstrapMounted = mount.ReadOnly && strings.Contains(mount.Subpath, "/turn/"+state.Turn.ID+"/")
		}
	}
	if !bootstrapMounted {
		t.Fatal("deployment bootstrap was not mounted read-only from exact Turn scratch")
	}
	if factory.engine.spec.Image != legacy.Contract().Image || sessions.request.Session.RuntimeProfileContentSHA256 != legacy.ContentSHA256() || sessions.request.Session.ID != state.Session.ID || sessions.request.Session.ACPSessionID != state.Session.ACPSessionID {
		t.Fatal("upgrade changed the retained image, contract, or Session identity")
	}
	if err := handle.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workspaces.cleanedTurnID != state.Turn.ID {
		t.Fatal("stopped retained environment did not release its exact bootstrap scratch")
	}
}
