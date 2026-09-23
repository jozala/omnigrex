package github_test

import (
	"strings"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestRenderSignature(t *testing.T) {
	got := githubapi.RenderSignature("implementation-specialist", "Developer")
	want := "_By Omnigrex: `implementation-specialist` [Developer]_"
	if got != want {
		t.Fatalf("RenderSignature() = %q, want %q", got, want)
	}
}

func TestRenderSignatureEscapesDisplayName(t *testing.T) {
	got := githubapi.RenderSignature("reviewer", "Lead*Reviewer_[x]")
	if strings.Contains(got, "*Reviewer_") || strings.Contains(got, "[x]") {
		t.Fatalf("RenderSignature() did not escape display name: %q", got)
	}
	if escaped := githubapi.RenderSignature("reviewer", "Lead ~~Reviewer~~"); strings.Contains(escaped, "~~") {
		t.Fatalf("RenderSignature() did not escape tilde: %q", escaped)
	}
}

func TestAppendSignature(t *testing.T) {
	signature := githubapi.RenderSignature("dev", "Developer")
	signed := githubapi.AppendSignature("hello", signature)
	if !strings.HasPrefix(signed, "hello\n\n_By Omnigrex:") || !strings.HasSuffix(signed, "_") {
		t.Fatalf("AppendSignature() = %q", signed)
	}
	if again := githubapi.AppendSignature(signed, signature); again != signed {
		t.Fatalf("AppendSignature() not idempotent: %q vs %q", again, signed)
	}
	if empty := githubapi.AppendSignature("", signature); !strings.HasPrefix(empty, "_By Omnigrex:") {
		t.Fatalf("AppendSignature() empty = %q", empty)
	}
}

func TestMatchesSignedBodyAcceptsSignedAndLegacy(t *testing.T) {
	signature := githubapi.RenderSignature("dev", "Developer")
	if !githubapi.MatchesSignedBody("agent text\n\n"+signature, "agent text") {
		t.Fatalf("MatchesSignedBody() rejected signed body")
	}
	if !githubapi.MatchesSignedBody("agent text", "agent text") {
		t.Fatalf("MatchesSignedBody() rejected legacy unsigned body")
	}
	if githubapi.MatchesSignedBody("other text\n\n"+signature, "agent text") {
		t.Fatalf("MatchesSignedBody() accepted mismatched body")
	}
}

func TestJoinPostedBodyMirrorsAPIAssembly(t *testing.T) {
	marker := "<!-- omnigrex:v1 workflow=workflow-1 -->"
	if got := githubapi.JoinPostedBody("hello", marker); got != "hello\n\n"+marker {
		t.Fatalf("JoinPostedBody() = %q", got)
	}
	if got := githubapi.JoinPostedBody("", marker); got != marker {
		t.Fatalf("JoinPostedBody() empty = %q", got)
	}
}

func TestCheckPostedBodyLength(t *testing.T) {
	if err := githubapi.CheckPostedBodyLength("hello", "<!-- marker -->"); err != nil {
		t.Fatalf("CheckPostedBodyLength() error = %v", err)
	}
	oversized := strings.Repeat("x", githubapi.MaxPostedBodyLength+1)
	if err := githubapi.CheckPostedBodyLength(oversized, ""); err == nil {
		t.Fatalf("CheckPostedBodyLength() accepted oversized body")
	}
	if err := githubapi.CheckFinalBodyLength(oversized); err == nil {
		t.Fatalf("CheckFinalBodyLength() accepted oversized body")
	}
}
