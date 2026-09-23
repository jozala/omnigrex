package agentprofile_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

type sourceCall struct {
	credential string
	owner      string
	repository string
	path       string
	commitSHA  string
}

type fakeSource struct {
	commitSHA    string
	resolveErr   error
	paths        []string
	listErr      error
	contents     map[string][]byte
	fetchErr     map[string]error
	resolveCalls []sourceCall
	listCalls    []sourceCall
	fetchCalls   []sourceCall
}

func (source *fakeSource) ResolveDefaultBranchCommit(_ context.Context, credential, owner, repository string) (string, error) {
	source.resolveCalls = append(source.resolveCalls, sourceCall{credential: credential, owner: owner, repository: repository})
	return source.commitSHA, source.resolveErr
}

func (source *fakeSource) ListRepositoryDirectoryFiles(_ context.Context, credential, owner, repository, path, commitSHA string) ([]string, error) {
	source.listCalls = append(source.listCalls, sourceCall{credential: credential, owner: owner, repository: repository, path: path, commitSHA: commitSHA})
	return source.paths, source.listErr
}

func (source *fakeSource) FetchRepositoryFile(_ context.Context, credential, owner, repository, path, commitSHA string) ([]byte, error) {
	source.fetchCalls = append(source.fetchCalls, sourceCall{credential: credential, owner: owner, repository: repository, path: path, commitSHA: commitSHA})
	return source.contents[path], source.fetchErr[path]
}

func TestLoaderDiscoversAllProfilesAtOneCommit(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	source := &fakeSource{
		commitSHA: commitSHA,
		paths: []string{
			".omnigrex/team/implementation.md",
			".omnigrex/team/quality.md",
			".omnigrex/team/README",
			".omnigrex/team/ignored.MD",
			".omnigrex/team/notes.md.backup",
		},
		contents: map[string][]byte{
			".omnigrex/team/implementation.md": validProfileContent("primary-developer", role.Developer, "openai/gpt-5.2", "Develop the Work Item.\n"),
			".omnigrex/team/quality.md":        validProfileContent("strict-reviewer", role.Reviewer, "anthropic/claude-sonnet-4", "Review the Change Proposal.\n"),
		},
		fetchErr: make(map[string]error),
	}

	snapshot, err := agentprofile.NewLoader(source, role.BuiltinPolicyCatalog()).Load(context.Background(), "installation-secret", "acme", "widgets")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.CommitSHA() != commitSHA {
		t.Errorf("CommitSHA() = %q, want %q", snapshot.CommitSHA(), commitSHA)
	}
	selection, err := snapshot.Select(agentprofile.SingletonSelector{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	developerName, ok := selection.Profile(role.Developer)
	developer, found := snapshot.Profile(developerName)
	if !ok || !found || developer.Name() != "primary-developer" || developer.Path() != ".omnigrex/team/implementation.md" {
		t.Errorf("selected Developer = (%#v, %t, %t)", developer, ok, found)
	}
	reviewerName, ok := selection.Profile(role.Reviewer)
	reviewer, found := snapshot.Profile(reviewerName)
	if !ok || !found || reviewer.Name() != "strict-reviewer" || reviewer.Model() != "anthropic/claude-sonnet-4" {
		t.Errorf("selected Reviewer = (%#v, %t, %t)", reviewer, ok, found)
	}
	if profile, found := snapshot.Profile("primary-developer"); !found || profile.ContentSHA256() != developer.ContentSHA256() {
		t.Errorf("Profile(primary-developer) = (%#v, %t)", profile, found)
	}
	if _, found := snapshot.Profile("implementation"); found {
		t.Error("Profile path stem was incorrectly used as the Profile name")
	}
	if len(source.resolveCalls) != 1 || len(source.listCalls) != 1 {
		t.Fatalf("source calls = resolve %#v, list %#v", source.resolveCalls, source.listCalls)
	}
	wantListCall := sourceCall{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team", commitSHA: commitSHA}
	if source.listCalls[0] != wantListCall {
		t.Errorf("list call = %#v, want %#v", source.listCalls[0], wantListCall)
	}
	wantFetchCalls := []sourceCall{
		{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team/implementation.md", commitSHA: commitSHA},
		{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team/quality.md", commitSHA: commitSHA},
	}
	if !reflect.DeepEqual(source.fetchCalls, wantFetchCalls) {
		t.Errorf("fetch calls = %#v\nwant = %#v", source.fetchCalls, wantFetchCalls)
	}
	if strings.Contains(string(developer.CanonicalJSON()), "installation-secret") || strings.Contains(string(reviewer.CanonicalJSON()), "installation-secret") {
		t.Error("canonical profile JSON contains repository credential")
	}
}

func TestLoaderRejectsInvalidDiscoveredProfileSets(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	baseContents := map[string][]byte{
		".omnigrex/team/developer.md": validProfileContent("developer", role.Developer, "openai/gpt-5.2", "Develop.\n"),
		".omnigrex/team/reviewer.md":  validProfileContent("reviewer", role.Reviewer, "anthropic/reviewer", "Review.\n"),
	}
	tests := []struct {
		name     string
		paths    []string
		contents map[string][]byte
	}{
		{name: "no markdown profiles"},
		{name: "missing policy Role", paths: []string{".omnigrex/team/developer.md"}, contents: baseContents},
		{name: "multiple profiles for Role", paths: []string{".omnigrex/team/developer.md", ".omnigrex/team/alternate.md", ".omnigrex/team/reviewer.md"}, contents: map[string][]byte{
			".omnigrex/team/developer.md": validProfileContent("developer", role.Developer, "openai/gpt-5.2", "Develop.\n"),
			".omnigrex/team/alternate.md": validProfileContent("alternate", role.Developer, "openai/gpt-5.2", "Develop.\n"),
			".omnigrex/team/reviewer.md":  baseContents[".omnigrex/team/reviewer.md"],
		}},
		{name: "duplicate names", paths: []string{".omnigrex/team/developer.md", ".omnigrex/team/reviewer.md"}, contents: map[string][]byte{
			".omnigrex/team/developer.md": baseContents[".omnigrex/team/developer.md"],
			".omnigrex/team/reviewer.md":  validProfileContent("developer", role.Reviewer, "anthropic/reviewer", "Review.\n"),
		}},
		{name: "unreferenced Role", paths: []string{".omnigrex/team/developer.md", ".omnigrex/team/reviewer.md", ".omnigrex/team/architect.md"}, contents: map[string][]byte{
			".omnigrex/team/developer.md": baseContents[".omnigrex/team/developer.md"],
			".omnigrex/team/reviewer.md":  baseContents[".omnigrex/team/reviewer.md"],
			".omnigrex/team/architect.md": validProfileContent("architect", role.ID("ARCHITECT"), "openai/gpt-5.2", "Design.\n"),
		}},
		{name: "unsafe listed path", paths: []string{".omnigrex/team/nested/developer.md"}, contents: map[string][]byte{
			".omnigrex/team/nested/developer.md": baseContents[".omnigrex/team/developer.md"],
		}},
		{name: "invalid profile", paths: []string{".omnigrex/team/developer.md"}, contents: map[string][]byte{
			".omnigrex/team/developer.md": []byte("invalid"),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeSource{commitSHA: commitSHA, paths: test.paths, contents: test.contents, fetchErr: map[string]error{}}
			snapshot, err := agentprofile.NewLoader(source, role.BuiltinPolicyCatalog()).Load(context.Background(), "token", "acme", "widgets")
			if err == nil {
				_, err = snapshot.Select(agentprofile.SingletonSelector{})
			}
			if err == nil {
				t.Fatal("Load() error = nil, want rejection")
			}
		})
	}
}

func TestLoaderStopsOnSourceFailures(t *testing.T) {
	failure := errors.New("source unavailable")
	validCommit := strings.Repeat("a", 40)
	tests := []struct {
		name          string
		source        *fakeSource
		wantListCalls int
		wantFetchCall int
	}{
		{name: "nil source"},
		{name: "resolution failure", source: &fakeSource{resolveErr: failure}},
		{name: "blank commit", source: &fakeSource{commitSHA: "  "}},
		{name: "unsafe commit", source: &fakeSource{commitSHA: "main~1"}},
		{name: "abbreviated commit", source: &fakeSource{commitSHA: "abc123"}},
		{name: "list failure", source: &fakeSource{commitSHA: validCommit, listErr: failure}, wantListCalls: 1},
		{name: "fetch failure", source: &fakeSource{
			commitSHA: validCommit,
			paths:     []string{".omnigrex/team/developer.md"},
			contents:  map[string][]byte{},
			fetchErr:  map[string]error{".omnigrex/team/developer.md": failure},
		}, wantListCalls: 1, wantFetchCall: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var source agentprofile.Source
			if test.source != nil {
				source = test.source
			}
			loader := agentprofile.NewLoader(source, role.BuiltinPolicyCatalog())
			if _, err := loader.Load(context.Background(), "token", "acme", "widgets"); err == nil {
				t.Fatal("Load() error = nil, want failure")
			}
			if test.source != nil && (len(test.source.listCalls) != test.wantListCalls || len(test.source.fetchCalls) != test.wantFetchCall) {
				t.Errorf("calls = list %d, fetch %d; want list %d, fetch %d", len(test.source.listCalls), len(test.source.fetchCalls), test.wantListCalls, test.wantFetchCall)
			}
		})
	}
}
