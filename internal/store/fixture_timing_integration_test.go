//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
)

// IDs are process-local counters, never container names or connection strings.
var fixtureTimingSequence atomic.Uint64
var fixtureTimingDatabases sync.Map

type fixtureTimingLogger interface {
	Helper()
	Logf(string, ...any)
}

type fixtureTimingRecord struct {
	Fixture   string `json:"fixture"`
	Phase     string `json:"phase"`
	ElapsedNS int64  `json:"elapsed_ns"`
	Outcome   string `json:"outcome"`
}

func beginFixturePhase(t fixtureTimingLogger, fixture, phase string) func(error) {
	t.Helper()
	start := time.Now()
	return func(err error) {
		t.Helper()
		record := fixtureTimingRecord{Fixture: fixture, Phase: phase, ElapsedNS: time.Since(start).Nanoseconds(), Outcome: "ok"}
		if err != nil {
			record.Outcome = "error"
		}
		data, _ := json.Marshal(record)
		t.Logf("fixture_timing %s", data)
	}
}

func openTimedStore(t fixtureTimingLogger, ctx context.Context, databaseURL, passwordFile string, config store.Config) (database *store.Store, err error) {
	t.Helper()
	var fixture string
	if parsed, parseErr := url.Parse(databaseURL); parseErr == nil {
		if value, found := fixtureTimingDatabases.Load(parsed.Host + "\x00" + parsed.Path); found {
			fixture = value.(string)
		}
	}
	finish := beginFixturePhase(t, fixture, "store_open")
	defer func() { finish(err) }()
	return store.Open(ctx, databaseURL, passwordFile, config)
}

type fixtureTimingBuffer struct{ strings.Builder }

func (*fixtureTimingBuffer) Helper() {}
func (buffer *fixtureTimingBuffer) Logf(format string, args ...any) {
	fmt.Fprintf(&buffer.Builder, format, args...)
}

func TestFixtureTimingRecordsSafeMetadataOnSuccessAndFailure(t *testing.T) {
	for _, failure := range []error{nil, fmt.Errorf("private-database-password")} {
		var logger fixtureTimingBuffer
		finish := beginFixturePhase(&logger, "1", "postgres_ready")
		finish(failure)
		var record fixtureTimingRecord
		if err := json.Unmarshal([]byte(strings.TrimPrefix(logger.String(), "fixture_timing ")), &record); err != nil {
			t.Fatal(err)
		}
		want := "ok"
		if failure != nil {
			want = "error"
		}
		if record.Fixture != "1" || record.Phase != "postgres_ready" || record.Outcome != want || record.ElapsedNS < 0 || strings.Contains(logger.String(), "private") {
			t.Fatalf("unsafe or incorrect timing: %s", logger.String())
		}
	}
}

func TestTimedStoreOpenPreservesErrorsWithoutLoggingConnectionDetails(t *testing.T) {
	var logger fixtureTimingBuffer
	database, err := openTimedStore(&logger, context.Background(), "postgres://private-user:private-password@localhost/private-database", "/private-secret", store.Config{})
	if database != nil || err == nil || !strings.Contains(err.Error(), "invalid Workflow Reducer") {
		t.Fatalf("store validation changed: %v %v", database, err)
	}
	if !strings.Contains(logger.String(), `"outcome":"error"`) || !strings.Contains(logger.String(), `"phase":"store_open"`) || strings.Contains(logger.String(), "private") {
		t.Fatalf("unsafe or missing timing: %s", logger.String())
	}
}

func TestStoreOpenTimingDistinguishesDatabasesOnOneServer(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		key := "fixture.invalid:5432\x00/" + name
		fixtureTimingDatabases.Store(key, name+"-fixture")
		t.Cleanup(func() { fixtureTimingDatabases.Delete(key) })
		var logger fixtureTimingBuffer
		_, err := openTimedStore(&logger, context.Background(), "postgres://private:private@fixture.invalid:5432/"+name+"?application_name=private", "/private-secret", store.Config{})
		if err == nil {
			t.Fatal("invalid configuration unexpectedly opened a store")
		}
		var record fixtureTimingRecord
		if err := json.Unmarshal([]byte(strings.TrimPrefix(logger.String(), "fixture_timing ")), &record); err != nil {
			t.Fatal(err)
		}
		if record.Fixture != name+"-fixture" || strings.Contains(logger.String(), "private") {
			t.Fatalf("incorrect or unsafe fixture attribution: %s", logger.String())
		}
	}
}
