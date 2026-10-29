package migrations

import (
	"crypto/sha256"
	"testing"
	"testing/fstest"
)

func TestDiscoverOrdersMigrationsByVersion(t *testing.T) {
	files := fstest.MapFS{"000010_tenth.sql": {Data: []byte("SELECT 10;")}, "000002_second.sql": {Data: []byte("SELECT 2;")}, "README.md": {Data: []byte("not a migration")}}
	got, err := discover(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].version != 2 || got[0].name != "second" || got[1].version != 10 || got[1].name != "tenth" {
		t.Fatalf("migration order = %#v", got)
	}
}

func TestDiscoverHashesExactMigrationContents(t *testing.T) {
	for _, content := range []string{"SELECT 2;", "SELECT 2;\n"} {
		got, err := discover(fstest.MapFS{"000001_first.sql": {Data: []byte(content)}})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].checksum != sha256.Sum256([]byte(content)) {
			t.Fatalf("incorrect checksum for %q", content)
		}
	}
}

func TestDiscoverRejectsInvalidMigrationNamesAndDuplicateVersions(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"invalid":   {"migration.sql": {Data: []byte("SELECT 1;")}},
		"zero":      {"000000_zero.sql": {Data: []byte("SELECT 1;")}},
		"duplicate": {"000001_first.sql": {Data: []byte("SELECT 1;")}, "000001_duplicate.sql": {Data: []byte("SELECT 2;")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := discover(files); err == nil {
				t.Fatal("invalid migration set was accepted")
			}
		})
	}
}

func TestEmbeddedMigrationsCanBeDiscovered(t *testing.T) {
	if _, err := discover(Files); err != nil {
		t.Fatalf("embedded migrations prevent controller startup: %v", err)
	}
}
