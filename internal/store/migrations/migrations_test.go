package migrations

import (
	"encoding/hex"
	"strings"
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

func TestDiscoverHashesExactMigrationContents(t *testing.T) {
	files := fstest.MapFS{
		"000001_first.sql": {Data: []byte("SELECT 2;")},
	}
	want, err := hex.DecodeString("8e7003d62f9d8cbd28da2f243bb0d215bfd4622c716be09be89a8764d9f4c7cb")
	if err != nil {
		t.Fatal(err)
	}

	discovered, err := discover(files)
	if err != nil {
		t.Fatalf("discover() error = %v", err)
	}
	if string(discovered[0].checksum[:]) != string(want) {
		t.Errorf("migration checksum = %x, want %x", discovered[0].checksum, want)
	}
}

func TestWebhookAttemptLimitMigrationsSeparateFastDefaultFromBackfill(t *testing.T) {
	addition, err := Files.ReadFile("000014_bounded_webhook_delivery_attempts.sql")
	if err != nil {
		t.Fatal(err)
	}
	additionSQL := string(addition)
	if strings.Contains(additionSQL, "UPDATE webhook_deliveries") || !strings.Contains(additionSQL, "DEFAULT 3") || !strings.Contains(additionSQL, "NOT VALID") {
		t.Errorf("migration 14 must use a fast default and unvalidated constraint without rewriting deliveries:\n%s", additionSQL)
	}

	backfill, err := Files.ReadFile("000015_backfill_webhook_delivery_attempts.sql")
	if err != nil {
		t.Fatal(err)
	}
	backfillSQL := string(backfill)
	if !strings.Contains(backfillSQL, "UPDATE webhook_deliveries") || !strings.Contains(backfillSQL, "WHERE attempt_count >= 3") || !strings.Contains(backfillSQL, "VALIDATE CONSTRAINT") {
		t.Errorf("migration 15 must narrowly backfill exceptional rows before validating the constraint:\n%s", backfillSQL)
	}
}

func TestNormalizedEventFailureMigrationsSeparateSchemaChangeFromValidation(t *testing.T) {
	addition, err := Files.ReadFile("000016_failed_historical_normalized_events.sql")
	if err != nil {
		t.Fatal(err)
	}
	additionSQL := string(addition)
	if !strings.Contains(additionSQL, "DEFAULT 0") || !strings.Contains(additionSQL, "DEFAULT 3") ||
		!strings.Contains(additionSQL, "NOT VALID") || strings.Contains(additionSQL, "VALIDATE CONSTRAINT") {
		t.Errorf("migration 16 must use fast defaults and unvalidated constraints:\n%s", additionSQL)
	}

	validation, err := Files.ReadFile("000017_validate_normalized_event_failures.sql")
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(validation), "VALIDATE CONSTRAINT"); count != 3 {
		t.Errorf("migration 17 validates %d constraints, want 3", count)
	}
}
