package agentprofile_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
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
	contents     map[string][]byte
	fetchErr     map[string]error
	resolveCalls []sourceCall
	fetchCalls   []sourceCall
}

func (source *fakeSource) ResolveDefaultBranchCommit(_ context.Context, credential, owner, repository string) (string, error) {
	source.resolveCalls = append(source.resolveCalls, sourceCall{credential: credential, owner: owner, repository: repository})
	return source.commitSHA, source.resolveErr
}

func (source *fakeSource) FetchRepositoryFile(_ context.Context, credential, owner, repository, path, commitSHA string) ([]byte, error) {
	source.fetchCalls = append(source.fetchCalls, sourceCall{credential: credential, owner: owner, repository: repository, path: path, commitSHA: commitSHA})
	return source.contents[path], source.fetchErr[path]
}

func TestLoaderLoadsBothProfilesFromOneDefaultBranchCommit(t *testing.T) {
	const commitSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	developer := profileContent("openai/gpt-5.2", "Develop the Work Item.\n")
	reviewer := profileContent("anthropic/claude-sonnet-4", "Review the Change Proposal.\n")
	source := &fakeSource{
		commitSHA: commitSHA,
		contents: map[string][]byte{
			".omnigrex/team/developer.md": developer,
			".omnigrex/team/reviewer.md":  reviewer,
		},
		fetchErr: make(map[string]error),
	}

	snapshot, err := agentprofile.NewLoader(source).Load(context.Background(), "installation-secret", "acme", "widgets")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.CommitSHA() != commitSHA {
		t.Errorf("CommitSHA() = %q, want %q", snapshot.CommitSHA(), commitSHA)
	}
	if snapshot.Developer().Name() != agentprofile.Developer || snapshot.Developer().Model() != "openai/gpt-5.2" {
		t.Errorf("Developer() = (%q, %q)", snapshot.Developer().Name(), snapshot.Developer().Model())
	}
	if snapshot.Reviewer().Name() != agentprofile.Reviewer || snapshot.Reviewer().Model() != "anthropic/claude-sonnet-4" {
		t.Errorf("Reviewer() = (%q, %q)", snapshot.Reviewer().Name(), snapshot.Reviewer().Model())
	}
	profile, ok := snapshot.Profile(agentprofile.Reviewer)
	if !ok || profile.ContentSHA256() != snapshot.Reviewer().ContentSHA256() {
		t.Errorf("Profile(Reviewer) = (%#v, %t)", profile, ok)
	}
	if _, ok := snapshot.Profile(agentprofile.Name("feature-branch")); ok {
		t.Error("Profile(unknown) unexpectedly succeeded")
	}
	if len(source.resolveCalls) != 1 {
		t.Fatalf("resolve calls = %#v, want exactly one", source.resolveCalls)
	}
	wantFetchCalls := []sourceCall{
		{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team/developer.md", commitSHA: commitSHA},
		{credential: "installation-secret", owner: "acme", repository: "widgets", path: ".omnigrex/team/reviewer.md", commitSHA: commitSHA},
	}
	if !reflect.DeepEqual(source.fetchCalls, wantFetchCalls) {
		t.Errorf("fetch calls = %#v\nwant = %#v", source.fetchCalls, wantFetchCalls)
	}
	if strings.Contains(string(snapshot.Developer().CanonicalJSON()), "installation-secret") || strings.Contains(string(snapshot.Reviewer().CanonicalJSON()), "installation-secret") {
		t.Error("canonical profile JSON contains repository credential")
	}
}

func TestLoaderStopsOnResolutionAndProfileFailures(t *testing.T) {
	failure := errors.New("source unavailable")
	tests := []struct {
		name          string
		source        *fakeSource
		wantFetchCall int
	}{
		{name: "nil source"},
		{name: "resolution failure", source: &fakeSource{resolveErr: failure}, wantFetchCall: 0},
		{name: "blank commit", source: &fakeSource{commitSHA: "  "}, wantFetchCall: 0},
		{name: "unsafe commit", source: &fakeSource{commitSHA: "main~1"}, wantFetchCall: 0},
		{name: "abbreviated commit", source: &fakeSource{commitSHA: "abc123"}, wantFetchCall: 0},
		{name: "Developer fetch failure", source: &fakeSource{commitSHA: strings.Repeat("a", 40), contents: map[string][]byte{}, fetchErr: map[string]error{".omnigrex/team/developer.md": failure}}, wantFetchCall: 1},
		{name: "invalid Developer profile", source: &fakeSource{commitSHA: strings.Repeat("a", 40), contents: map[string][]byte{".omnigrex/team/developer.md": []byte("invalid")}, fetchErr: map[string]error{}}, wantFetchCall: 1},
		{name: "Reviewer fetch failure", source: &fakeSource{commitSHA: strings.Repeat("a", 40), contents: map[string][]byte{".omnigrex/team/developer.md": profileContent("openai/gpt-5.2", "Develop.\n")}, fetchErr: map[string]error{".omnigrex/team/reviewer.md": failure}}, wantFetchCall: 2},
		{name: "invalid Reviewer profile", source: &fakeSource{commitSHA: strings.Repeat("a", 40), contents: map[string][]byte{".omnigrex/team/developer.md": profileContent("openai/gpt-5.2", "Develop.\n"), ".omnigrex/team/reviewer.md": []byte("invalid")}, fetchErr: map[string]error{}}, wantFetchCall: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var source agentprofile.Source
			if test.source != nil {
				source = test.source
			}
			loader := agentprofile.NewLoader(source)
			if _, err := loader.Load(context.Background(), "token", "acme", "widgets"); err == nil {
				t.Fatal("Load() error = nil, want failure")
			}
			if test.source != nil && len(test.source.fetchCalls) != test.wantFetchCall {
				t.Errorf("fetch calls = %d, want %d", len(test.source.fetchCalls), test.wantFetchCall)
			}
		})
	}
}

func profileContent(model, instructions string) []byte {
	return []byte(fmt.Sprintf("---\nruntime: opencode-acp/v1\nmodel: %s\nsteps: 50\npermissions:\n  read: allow\n---\n%s", model, instructions))
}
