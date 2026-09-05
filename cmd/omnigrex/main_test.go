package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadNonemptyJSONObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	const credential = `{"provider":{"apiKey":"credential-sentinel"}}`
	if err := os.WriteFile(path, []byte(credential), 0o600); err != nil {
		t.Fatalf("write provider credential fixture: %v", err)
	}

	got, err := readNonemptyJSONObject(path)
	if err != nil {
		t.Fatalf("readNonemptyJSONObject() error = %v", err)
	}
	if string(got) != credential {
		t.Fatal("readNonemptyJSONObject() did not preserve the raw JSON")
	}
	zeroBytes(got)
}

func TestReadNonemptyJSONObjectRejectsInvalidContentWithoutDisclosure(t *testing.T) {
	for _, content := range []string{"", "null", "[]", "{}", `{"secret":"credential-sentinel"} trailing`} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "provider.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write provider credential fixture: %v", err)
			}

			got, err := readNonemptyJSONObject(path)
			if err == nil || got != nil {
				t.Fatalf("readNonemptyJSONObject() = (%q, %v), want validation error", got, err)
			}
			if content != "" && strings.Contains(err.Error(), content) || strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("validation error disclosed file content: %v", err)
			}
		})
	}
}
