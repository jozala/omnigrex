package agentturn_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

const (
	testCommitSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImage     = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type sourceCall struct {
	credential string
	owner      string
	repository string
	path       string
	commitSHA  string
}

type profileSource struct {
	commitSHA   string
	contents    map[string][]byte
	resolveCall []sourceCall
	fetchCalls  []sourceCall
}

func (source *profileSource) ResolveDefaultBranchCommit(_ context.Context, credential, owner, repository string) (string, error) {
	source.resolveCall = append(source.resolveCall, sourceCall{credential: credential, owner: owner, repository: repository})
	return source.commitSHA, nil
}

func (source *profileSource) FetchRepositoryFile(_ context.Context, credential, owner, repository, path, commitSHA string) ([]byte, error) {
	source.fetchCalls = append(source.fetchCalls, sourceCall{
		credential: credential,
		owner:      owner,
		repository: repository,
		path:       path,
		commitSHA:  commitSHA,
	})
	return source.contents[path], nil
}

type preparationStore struct {
	selected workflow.Role
	calls    []store.AgentTurnPreparationSpec
	err      error
}

func (database *preparationStore) PrepareAgentTurn(_ context.Context, _ store.JobLease, spec store.AgentTurnPreparationSpec) (store.AgentTurnPreparationCommit, error) {
	database.calls = append(database.calls, clonePreparationSpec(spec))
	if database.err != nil {
		return store.AgentTurnPreparationCommit{}, database.err
	}
	selected := spec.Developer
	if database.selected == workflow.RoleReviewer {
		selected = spec.Reviewer
	}
	return store.AgentTurnPreparationCommit{Assignment: store.AgentAssignment{
		AssignmentRuntimeBinding: selected.Binding,
		Role:                     database.selected,
	}}, nil
}

type profileLoaderFunc func(context.Context, string, string, string) (agentprofile.Snapshot, error)

func (load profileLoaderFunc) Load(ctx context.Context, credential, owner, repository string) (agentprofile.Snapshot, error) {
	return load(ctx, credential, owner, repository)
}

type runtimeRegistryFunc func(string, string) (runtimeprofile.Profile, error)

func (resolve runtimeRegistryFunc) Resolve(name, version string) (runtimeprofile.Profile, error) {
	return resolve(name, version)
}

type secretBearingPreparationError struct {
	message string
}

func (err *secretBearingPreparationError) Error() string { return err.message }

func TestPrepareLoadsOneProfileSnapshotAndPreparesSelectedRole(t *testing.T) {
	developerContent := agentProfileContent("openai/gpt-5.2", "high", 80, "Develop the Work Item.\n")
	reviewerContent := agentProfileContent("anthropic/claude-sonnet-4", "", 40, "Review the Change Proposal.\n")
	source := &profileSource{
		commitSHA: testCommitSHA,
		contents: map[string][]byte{
			".omnigrex/team/developer.md": developerContent,
			".omnigrex/team/reviewer.md":  reviewerContent,
		},
	}
	runtime := runtimeProfile(t, testImage)
	registry, err := runtimeprofile.NewRegistry(runtime)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	database := &preparationStore{selected: workflow.RoleReviewer}
	lease := store.JobLease{Job: store.Job{ID: "preparation-job", JobSpec: store.JobSpec{Kind: store.PrepareAgentTurnJobKind}, Status: store.JobLeased}, Attempt: 1}

	result, err := agentturn.NewPreparer(agentprofile.NewLoader(source), registry, database).Prepare(context.Background(), agentturn.Request{
		Lease:                  lease,
		InstallationCredential: "installation-secret",
		RepositoryOwner:        "acme",
		RepositoryName:         "widgets",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if len(source.resolveCall) != 1 {
		t.Fatalf("default-branch resolution calls = %#v, want one", source.resolveCall)
	}
	wantFetchCalls := []sourceCall{
		{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team/developer.md", commitSHA: testCommitSHA},
		{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team/reviewer.md", commitSHA: testCommitSHA},
	}
	if !reflect.DeepEqual(source.fetchCalls, wantFetchCalls) {
		t.Fatalf("profile fetch calls = %#v, want %#v", source.fetchCalls, wantFetchCalls)
	}
	if len(database.calls) != 1 {
		t.Fatalf("PrepareAgentTurn() calls = %d, want one", len(database.calls))
	}

	wantBinding := func(name string) store.AssignmentRuntimeBinding {
		return store.AssignmentRuntimeBinding{
			AgentProfileName:            name,
			RuntimeProfileName:          "opencode-acp",
			RuntimeProfileVersion:       "v1",
			RuntimeProfileContentSHA256: runtime.ContentSHA256(),
			RuntimeImageDigest:          testImage,
		}
	}
	spec := database.calls[0]
	assertRolePreparation(t, spec.Developer, wantBinding("developer"), testCommitSHA, developerContent)
	assertRolePreparation(t, spec.Reviewer, wantBinding("reviewer"), testCommitSHA, reviewerContent)
	if strings.Contains(string(spec.Developer.Profile.Config), "installation-secret") || strings.Contains(string(spec.Reviewer.Profile.Config), "installation-secret") {
		t.Fatal("durable Agent Profile snapshots contain the installation credential")
	}
	if result.Commit.Assignment.Role != workflow.RoleReviewer || result.Commit.Assignment.AssignmentRuntimeBinding != wantBinding("reviewer") {
		t.Fatalf("selected Store commit = %#v", result.Commit)
	}
	if !reflect.DeepEqual(result.RuntimeProfile.Contract(), runtime.Contract()) {
		t.Fatalf("selected Runtime Profile = %#v, want %#v", result.RuntimeProfile.Contract(), runtime.Contract())
	}
}

func TestPrepareReturnsRoleSelectedByStore(t *testing.T) {
	for _, selected := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(selected), func(t *testing.T) {
			source := validProfileSource()
			registry, err := runtimeprofile.NewRegistry(runtimeProfile(t, testImage))
			if err != nil {
				t.Fatal(err)
			}
			database := &preparationStore{selected: selected}

			result, err := agentturn.NewPreparer(agentprofile.NewLoader(source), registry, database).Prepare(context.Background(), validRequest())
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if result.Commit.Assignment.Role != selected {
				t.Fatalf("selected Role = %q, want %q", result.Commit.Assignment.Role, selected)
			}
			wantName := "developer"
			if selected == workflow.RoleReviewer {
				wantName = "reviewer"
			}
			if result.Commit.Assignment.AgentProfileName != wantName {
				t.Fatalf("selected Agent Profile = %q, want %q", result.Commit.Assignment.AgentProfileName, wantName)
			}
			if result.RuntimeProfile.Contract().Image != testImage {
				t.Fatalf("selected runtime image = %q, want exact %q", result.RuntimeProfile.Contract().Image, testImage)
			}
		})
	}
}

func TestPrepareRejectsMissingDependenciesAndInvalidRequestBeforeLoadingProfiles(t *testing.T) {
	runtime := runtimeProfile(t, testImage)
	registry, err := runtimeprofile.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest()
	unusedLoader := profileLoaderFunc(func(context.Context, string, string, string) (agentprofile.Snapshot, error) {
		t.Fatal("profile loader called")
		return agentprofile.Snapshot{}, nil
	})
	database := &preparationStore{selected: workflow.RoleDeveloper}

	tests := []struct {
		name     string
		preparer *agentturn.Preparer
		request  agentturn.Request
		want     error
	}{
		{name: "nil Preparer", request: request, want: agentturn.ErrDependencyNil},
		{name: "nil loader", preparer: agentturn.NewPreparer(nil, registry, database), request: request, want: agentturn.ErrDependencyNil},
		{name: "nil registry", preparer: agentturn.NewPreparer(unusedLoader, nil, database), request: request, want: agentturn.ErrDependencyNil},
		{name: "nil Store", preparer: agentturn.NewPreparer(unusedLoader, registry, nil), request: request, want: agentturn.ErrDependencyNil},
		{name: "blank credential", preparer: agentturn.NewPreparer(unusedLoader, registry, database), request: mutateRequest(request, func(value *agentturn.Request) { value.InstallationCredential = " \t" }), want: agentturn.ErrInvalidRequest},
		{name: "blank owner", preparer: agentturn.NewPreparer(unusedLoader, registry, database), request: mutateRequest(request, func(value *agentturn.Request) { value.RepositoryOwner = " " }), want: agentturn.ErrInvalidRequest},
		{name: "blank repository", preparer: agentturn.NewPreparer(unusedLoader, registry, database), request: mutateRequest(request, func(value *agentturn.Request) { value.RepositoryName = "" }), want: agentturn.ErrInvalidRequest},
		{name: "wrong Job kind", preparer: agentturn.NewPreparer(unusedLoader, registry, database), request: mutateRequest(request, func(value *agentturn.Request) { value.Lease.Kind = store.RunAgentTurnJobKind }), want: agentturn.ErrInvalidRequest},
		{name: "Job not leased", preparer: agentturn.NewPreparer(unusedLoader, registry, database), request: mutateRequest(request, func(value *agentturn.Request) { value.Lease.Status = store.JobAvailable }), want: agentturn.ErrInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.preparer.Prepare(context.Background(), test.request)
			if err == nil || !strings.Contains(err.Error(), test.want.Error()) || !githubapi.ExtractSafeErrorMetadata(err).Permanent {
				t.Fatalf("Prepare() error = %v, want %v", err, test.want)
			}
			if errors.Is(err, test.want) {
				t.Fatalf("Prepare() retained classified source error %v", test.want)
			}
		})
	}
	if len(database.calls) != 0 {
		t.Fatalf("PrepareAgentTurn() calls = %d, want none", len(database.calls))
	}
}

func TestPrepareRejectsMissingAndUnknownRuntimeBeforeStore(t *testing.T) {
	runtime := runtimeProfile(t, testImage)
	registry, err := runtimeprofile.NewRegistry(runtime)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		loader agentturn.ProfileLoader
		want   error
	}{
		{
			name: "missing runtime",
			loader: profileLoaderFunc(func(context.Context, string, string, string) (agentprofile.Snapshot, error) {
				return agentprofile.Snapshot{}, nil
			}),
			want: agentturn.ErrInvalidRuntimeProfileReference,
		},
		{
			name: "unknown runtime",
			loader: agentprofile.NewLoader(&profileSource{
				commitSHA: testCommitSHA,
				contents: map[string][]byte{
					".omnigrex/team/developer.md": agentProfileContentWithRuntime("unknown-runtime/v9", "openai/gpt-5.2", "Develop.\n"),
					".omnigrex/team/reviewer.md":  agentProfileContent("anthropic/reviewer", "", 40, "Review.\n"),
				},
			}),
			want: runtimeprofile.ErrNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := &preparationStore{selected: workflow.RoleDeveloper}
			_, err := agentturn.NewPreparer(test.loader, registry, database).Prepare(context.Background(), validRequest())
			if err == nil || !strings.Contains(err.Error(), test.want.Error()) || !githubapi.ExtractSafeErrorMetadata(err).Permanent {
				t.Fatalf("Prepare() error = %v, want %v", err, test.want)
			}
			if errors.Is(err, test.want) {
				t.Fatalf("Prepare() retained classified source error %v", test.want)
			}
			if len(database.calls) != 0 {
				t.Fatalf("PrepareAgentTurn() calls = %d, want none", len(database.calls))
			}
		})
	}
}

func TestPrepareRejectsResolvedRuntimeWithDifferentReferenceBeforeStore(t *testing.T) {
	source := &profileSource{
		commitSHA: testCommitSHA,
		contents: map[string][]byte{
			".omnigrex/team/developer.md": agentProfileContentWithRuntime("other-runtime/v9", "openai/gpt-5.2", "Develop.\n"),
			".omnigrex/team/reviewer.md":  agentProfileContent("anthropic/reviewer", "", 40, "Review.\n"),
		},
	}
	resolved := runtimeProfile(t, testImage)
	registry := runtimeRegistryFunc(func(name, version string) (runtimeprofile.Profile, error) {
		if name != "other-runtime" || version != "v9" {
			t.Fatalf("Resolve() = (%q, %q), want exact other-runtime/v9", name, version)
		}
		return resolved, nil
	})
	database := &preparationStore{selected: workflow.RoleDeveloper}

	_, err := agentturn.NewPreparer(agentprofile.NewLoader(source), registry, database).Prepare(context.Background(), validRequest())
	if err == nil || !strings.Contains(err.Error(), agentturn.ErrRuntimeProfileReferenceMismatch.Error()) || !githubapi.ExtractSafeErrorMetadata(err).Permanent {
		t.Fatalf("Prepare() error = %v, want ErrRuntimeProfileReferenceMismatch", err)
	}
	if errors.Is(err, agentturn.ErrRuntimeProfileReferenceMismatch) {
		t.Fatal("Prepare() retained ErrRuntimeProfileReferenceMismatch")
	}
	if len(database.calls) != 0 {
		t.Fatalf("PrepareAgentTurn() calls = %d, want none", len(database.calls))
	}
}

func TestPrepareRefreshesMutableProfileSnapshotsWithoutChangingBindings(t *testing.T) {
	source := validProfileSource()
	registry, err := runtimeprofile.NewRegistry(runtimeProfile(t, testImage))
	if err != nil {
		t.Fatal(err)
	}
	database := &preparationStore{selected: workflow.RoleDeveloper}
	preparer := agentturn.NewPreparer(agentprofile.NewLoader(source), registry, database)

	if _, err := preparer.Prepare(context.Background(), validRequest()); err != nil {
		t.Fatalf("Prepare() first error = %v", err)
	}
	source.commitSHA = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	source.contents[".omnigrex/team/developer.md"] = agentProfileContent("openai/gpt-5.3", "low", 25, "Apply the revised requirements.\n")
	source.contents[".omnigrex/team/reviewer.md"] = agentProfileContent("anthropic/claude-opus-4", "high", 60, "Review the revised proposal.\n")
	if _, err := preparer.Prepare(context.Background(), validRequest()); err != nil {
		t.Fatalf("Prepare() second error = %v", err)
	}
	if len(database.calls) != 2 {
		t.Fatalf("PrepareAgentTurn() calls = %d, want two", len(database.calls))
	}

	first, second := database.calls[0], database.calls[1]
	for role, preparations := range map[string][2]store.RolePreparation{
		"Developer": {first.Developer, second.Developer},
		"Reviewer":  {first.Reviewer, second.Reviewer},
	} {
		if preparations[0].Binding != preparations[1].Binding {
			t.Errorf("%s binding changed from %#v to %#v", role, preparations[0].Binding, preparations[1].Binding)
		}
		if reflect.DeepEqual(preparations[0].Profile, preparations[1].Profile) {
			t.Errorf("%s mutable snapshot did not change", role)
		}
		if preparations[0].Profile.CommitSHA != testCommitSHA || preparations[1].Profile.CommitSHA != source.commitSHA {
			t.Errorf("%s commits = (%q, %q)", role, preparations[0].Profile.CommitSHA, preparations[1].Profile.CommitSHA)
		}
	}
}

func TestPrepareRedactsCredentialWithoutRetainingSourceError(t *testing.T) {
	const credential = "installation-secret"
	sourceErr := &secretBearingPreparationError{message: "repository rejected " + credential}
	loader := profileLoaderFunc(func(_ context.Context, gotCredential, _, _ string) (agentprofile.Snapshot, error) {
		if gotCredential != credential {
			t.Fatalf("credential = %q", gotCredential)
		}
		return agentprofile.Snapshot{}, sourceErr
	})
	database := &preparationStore{selected: workflow.RoleDeveloper}
	request := validRequest()
	request.InstallationCredential = credential

	_, err := agentturn.NewPreparer(loader, runtimeprofile.Registry{}, database).Prepare(context.Background(), request)
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("Prepare() error leaked credential: %v", err)
	}
	assertPreparationSourceUnreachable(t, err, sourceErr)
	if len(database.calls) != 0 {
		t.Fatalf("PrepareAgentTurn() calls = %d, want none", len(database.calls))
	}
}

func TestPrepareRedactsCredentialFromStoreErrors(t *testing.T) {
	const credential = "installation-secret"
	storeErr := &secretBearingPreparationError{message: "Store failure exposed " + credential}
	registry, err := runtimeprofile.NewRegistry(runtimeProfile(t, testImage))
	if err != nil {
		t.Fatal(err)
	}
	database := &preparationStore{selected: workflow.RoleDeveloper, err: storeErr}

	result, err := agentturn.NewPreparer(agentprofile.NewLoader(validProfileSource()), registry, database).Prepare(context.Background(), validRequest())
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("Prepare() error leaked credential: %v", err)
	}
	assertPreparationSourceUnreachable(t, err, storeErr)
	if !reflect.DeepEqual(result, agentturn.Result{}) {
		t.Fatalf("Prepare() result on failure = %#v, want zero durable values", result)
	}
	if len(database.calls) != 1 {
		t.Fatalf("PrepareAgentTurn() calls = %d, want one", len(database.calls))
	}
}

func assertPreparationSourceUnreachable(t *testing.T, err error, source *secretBearingPreparationError) {
	t.Helper()
	if errors.Is(err, source) {
		t.Fatalf("sanitized error retained source through errors.Is: %v", err)
	}
	var recovered *secretBearingPreparationError
	if errors.As(err, &recovered) {
		t.Fatalf("sanitized error retained source through errors.As: %#v", recovered)
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if current == source {
			t.Fatalf("sanitized error retained source in Unwrap chain: %v", current)
		}
	}
}

func TestPrepareClassifiesAssignmentConfigurationConflict(t *testing.T) {
	registry, err := runtimeprofile.NewRegistry(runtimeProfile(t, testImage))
	if err != nil {
		t.Fatal(err)
	}
	database := &preparationStore{selected: workflow.RoleDeveloper, err: store.ErrAssignmentConfigurationConflict}

	result, err := agentturn.NewPreparer(agentprofile.NewLoader(validProfileSource()), registry, database).Prepare(context.Background(), validRequest())

	if !errors.Is(err, agentturn.ErrAssignmentConfigurationConflict) {
		t.Fatalf("Prepare() error = %v, want agentturn.ErrAssignmentConfigurationConflict", err)
	}
	var conflict *agentturn.AssignmentConfigurationConflictError
	if !errors.As(err, &conflict) || conflict.Preparation.Developer.Binding.AgentProfileName != "developer" || conflict.Preparation.Reviewer.Binding.AgentProfileName != "reviewer" {
		t.Fatalf("Prepare() conflict = %#v, want both credential-free Role preparations", conflict)
	}
	if !reflect.DeepEqual(result, agentturn.Result{}) {
		t.Fatalf("Prepare() result on conflict = %#v, want zero durable values", result)
	}
}

func assertRolePreparation(t *testing.T, got store.RolePreparation, wantBinding store.AssignmentRuntimeBinding, commitSHA string, content []byte) {
	t.Helper()
	if got.Binding != wantBinding {
		t.Errorf("binding = %#v, want %#v", got.Binding, wantBinding)
	}
	wantHash := sha256.Sum256(content)
	if got.Profile.CommitSHA != commitSHA || !reflect.DeepEqual(got.Profile.ContentSHA256, wantHash[:]) {
		t.Errorf("profile provenance = (%q, %x), want (%q, %x)", got.Profile.CommitSHA, got.Profile.ContentSHA256, commitSHA, wantHash)
	}
	name := agentprofile.Name(wantBinding.AgentProfileName)
	wantProfile, err := agentprofile.Parse(name, content)
	if err != nil {
		t.Fatalf("Parse() expected profile error = %v", err)
	}
	if !reflect.DeepEqual(got.Profile.Config, json.RawMessage(wantProfile.CanonicalJSON())) {
		t.Errorf("profile config = %s, want %s", got.Profile.Config, wantProfile.CanonicalJSON())
	}
}

func runtimeProfile(t *testing.T, image string) runtimeprofile.Profile {
	t.Helper()
	value, err := runtimeprofile.NewOpenCodeV1(image, runtimeprofile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatalf("NewOpenCodeV1() error = %v", err)
	}
	return value
}

func agentProfileContent(model, variant string, steps int, instructions string) []byte {
	variantLine := ""
	if variant != "" {
		variantLine = "variant: " + variant + "\n"
	}
	return []byte(fmt.Sprintf("---\nruntime: opencode-acp/v1\nmodel: %s\n%ssteps: %d\npermissions:\n  read: allow\n  edit: deny\n---\n%s", model, variantLine, steps, instructions))
}

func agentProfileContentWithRuntime(runtime, model, instructions string) []byte {
	return []byte(fmt.Sprintf("---\nruntime: %s\nmodel: %s\nsteps: 40\npermissions:\n  read: allow\n---\n%s", runtime, model, instructions))
}

func validRequest() agentturn.Request {
	return agentturn.Request{
		Lease: store.JobLease{
			Job: store.Job{
				ID:      "preparation-job",
				JobSpec: store.JobSpec{Kind: store.PrepareAgentTurnJobKind},
				Status:  store.JobLeased,
			},
			Attempt: 1,
		},
		InstallationCredential: "installation-secret",
		RepositoryOwner:        "acme",
		RepositoryName:         "widgets",
	}
}

func validProfileSource() *profileSource {
	return &profileSource{
		commitSHA: testCommitSHA,
		contents: map[string][]byte{
			".omnigrex/team/developer.md": agentProfileContent("openai/gpt-5.2", "high", 80, "Develop the Work Item.\n"),
			".omnigrex/team/reviewer.md":  agentProfileContent("anthropic/claude-sonnet-4", "", 40, "Review the Change Proposal.\n"),
		},
	}
}

func mutateRequest(request agentturn.Request, mutate func(*agentturn.Request)) agentturn.Request {
	mutate(&request)
	return request
}

func clonePreparationSpec(spec store.AgentTurnPreparationSpec) store.AgentTurnPreparationSpec {
	spec.Developer.Profile.ContentSHA256 = append([]byte(nil), spec.Developer.Profile.ContentSHA256...)
	spec.Developer.Profile.Config = append(json.RawMessage(nil), spec.Developer.Profile.Config...)
	spec.Reviewer.Profile.ContentSHA256 = append([]byte(nil), spec.Reviewer.Profile.ContentSHA256...)
	spec.Reviewer.Profile.Config = append(json.RawMessage(nil), spec.Reviewer.Profile.Config...)
	return spec
}
