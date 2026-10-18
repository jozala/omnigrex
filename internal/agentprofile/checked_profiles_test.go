package agentprofile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/role"
)

// TestCheckedInRepositoryProfilesUseProductionParser validates every direct
// Markdown Profile under .omnigrex/team in the source checkout with the same
// production parser as repository discovery, so invalid vocabulary is caught
// before release publication.
func TestCheckedInRepositoryProfilesUseProductionParser(t *testing.T) {
	root := findRepositoryRoot(t)
	team := filepath.Join(root, ".omnigrex", "team")
	entries, err := os.ReadDir(team)
	if err != nil {
		t.Fatalf("read checked-in Agent Profiles: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := ".omnigrex/team/" + entry.Name()
		content, err := os.ReadFile(filepath.Join(team, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		profile, err := agentprofile.Parse(path, content, role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatalf("checked-in Profile %s rejected: %v", path, err)
		}
		decoded, err := agentprofile.DecodeSnapshot(profile.CanonicalJSON(), role.BuiltinPolicyCatalog())
		if err != nil {
			t.Fatalf("checked-in Profile %s snapshot rejected: %v", path, err)
		}
		if decoded.Thinking() != profile.Thinking() || string(decoded.CanonicalJSON()) != string(profile.CanonicalJSON()) {
			t.Fatalf("checked-in Profile %s snapshot changed thinking %q to %q", path, profile.Thinking(), decoded.Thinking())
		}
		count++
	}
	if count == 0 {
		t.Fatal("no checked-in Agent Profiles found under .omnigrex/team")
	}
}

func findRepositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".omnigrex", "team")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root with .omnigrex/team not found")
		}
		dir = parent
	}
}
