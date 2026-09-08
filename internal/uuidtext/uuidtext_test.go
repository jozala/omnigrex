package uuidtext_test

import (
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jozala/omnigrex/internal/uuidtext"
)

func TestValidPreservesStandardTextSemantics(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "00000000-0000-0000-0000-000000000000", want: true},
		{value: "AAAAAAAA-AAAA-1AAA-0AAA-AAAAAAAAAAAA", want: true},
		{value: "ffffffff-ffff-ffff-ffff-ffffffffffff", want: true},
		{value: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
		{value: "urn:uuid:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", want: false},
		{value: "{aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa}", want: false},
		{value: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaag", want: false},
	}
	for _, test := range tests {
		if got := uuidtext.Valid(test.value); got != test.want {
			t.Errorf("Valid(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}

func TestCanonicalizeAcceptsStandardTextAndNormalizesCase(t *testing.T) {
	got, ok := uuidtext.Canonicalize("AAAAAAAA-AAAA-1AAA-0AAA-AAAAAAAAAAAA")
	if !ok || got != "aaaaaaaa-aaaa-1aaa-0aaa-aaaaaaaaaaaa" {
		t.Fatalf("Canonicalize() = (%q, %t)", got, ok)
	}
	if _, ok := uuidtext.Canonicalize("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); ok {
		t.Error("Canonicalize() accepted raw UUID text")
	}
}

func TestValidCanonicalNonNilRequiresExactLowercaseText(t *testing.T) {
	for _, value := range []string{
		"aaaaaaaa-aaaa-1aaa-0aaa-aaaaaaaaaaaa",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	} {
		if !uuidtext.ValidCanonicalNonNil(value) {
			t.Errorf("ValidCanonicalNonNil(%q) = false", value)
		}
	}
	for _, value := range []string{
		"00000000-0000-0000-0000-000000000000",
		"AAAAAAAA-AAAA-1AAA-0AAA-AAAAAAAAAAAA",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		if uuidtext.ValidCanonicalNonNil(value) {
			t.Errorf("ValidCanonicalNonNil(%q) = true", value)
		}
	}
}

func TestNewRandomReturnsCanonicalVersionFourUUID(t *testing.T) {
	value, err := uuidtext.NewRandom()
	if err != nil {
		t.Fatalf("NewRandom() error = %v", err)
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value || parsed == uuid.Nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("NewRandom() = %q (%v), want canonical non-nil RFC 4122 version 4 UUID", value, err)
	}
}

func TestNewRandomReturnsEntropyFailure(t *testing.T) {
	want := errors.New("entropy unavailable")
	uuid.SetRand(errorReader{err: want})
	t.Cleanup(func() { uuid.SetRand(rand.Reader) })
	if _, err := uuidtext.NewRandom(); !errors.Is(err, want) {
		t.Fatalf("NewRandom() error = %v, want %v", err, want)
	}
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}
