//go:build integration

package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
)

func TestUnsupportedStoredRuntimeContractBlocksAdmission(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	candidate, err := profile.NewPiV2("registry.example/omnigrex/pi@sha256:"+strings.Repeat("a", 64), profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	contract := candidate.Contract()
	contract.Version = "v1"
	content, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if _, err := pool.Exec(t.Context(), `INSERT INTO runtime_profile_contracts (content_sha256, contract) VALUES ($1, $2)`, hex.EncodeToString(digest[:]), content); err != nil {
		t.Fatal(err)
	}
	if err := databases[0].CheckPendingRuntimeProfileCompatibility(t.Context(), candidate); !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) {
		t.Fatalf("unsupported contract admission = %v", err)
	}
}
