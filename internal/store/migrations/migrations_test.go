package migrations

import (
	"testing"
	"testing/fstest"
)

func TestDiscoverOrdersMigrationsByVersion(t *testing.T) {
	files := fstest.MapFS{
		"000010_tenth.sql":  {Data: []byte("SELECT 10;")},
		"000002_second.sql": {Data: []byte("SELECT 2;")},
		"README.md":         {Data: []byte("not a migration")},
	}

	got, err := discover(files)
	if err != nil {
		t.Fatalf("discover() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(discover()) = %d, want 2", len(got))
	}
	if got[0].version != 2 || got[0].name != "second" {
		t.Errorf("first migration = (%d, %q), want (2, %q)", got[0].version, got[0].name, "second")
	}
	if got[1].version != 10 || got[1].name != "tenth" {
		t.Errorf("second migration = (%d, %q), want (10, %q)", got[1].version, got[1].name, "tenth")
	}
}
