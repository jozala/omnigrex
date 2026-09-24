package github

import (
	"strings"
	"testing"
)

func TestRenderSignature(t *testing.T) {
	got := RenderSignature("implementation-specialist", "Developer")
	want := "_By Omnigrex: `implementation-specialist` [Developer]_"
	if got != want {
		t.Fatalf("RenderSignature() = %q, want %q", got, want)
	}
}

func TestRenderSignatureEscapesDisplayName(t *testing.T) {
	got := RenderSignature("reviewer", "Lead*Reviewer_[x]")
	if strings.Contains(got, "*Reviewer_") || strings.Contains(got, "[x]") {
		t.Fatalf("RenderSignature() did not escape display name: %q", got)
	}
	if escaped := RenderSignature("reviewer", "Lead ~~Reviewer~~"); strings.Contains(escaped, "~~") {
		t.Fatalf("RenderSignature() did not escape tilde: %q", escaped)
	}
}

func TestAppendSignature(t *testing.T) {
	signature := RenderSignature("dev", "Developer")
	signed := AppendSignature("hello", signature)
	if !strings.HasPrefix(signed, "hello\n\n_By Omnigrex:") || !strings.HasSuffix(signed, "_") {
		t.Fatalf("AppendSignature() = %q", signed)
	}
	if again := AppendSignature(signed, signature); again != signed {
		t.Fatalf("AppendSignature() not idempotent: %q vs %q", again, signed)
	}
	if empty := AppendSignature("", signature); !strings.HasPrefix(empty, "_By Omnigrex:") {
		t.Fatalf("AppendSignature() empty = %q", empty)
	}
}

func TestJoinBodyPartsAssembly(t *testing.T) {
	marker := "<!-- omnigrex:v1 workflow=workflow-1 -->"
	if got := JoinPostedBody("hello", marker); got != "hello\n\n"+marker {
		t.Fatalf("JoinPostedBody() = %q", got)
	}
	if got := JoinPostedBody("", marker); got != marker {
		t.Fatalf("JoinPostedBody() empty = %q", got)
	}
	if got := JoinBodyParts("a", "", "b"); got != "a\n\nb" {
		t.Fatalf("JoinBodyParts() = %q", got)
	}
}

func TestCheckPostedBodyLength(t *testing.T) {
	if err := CheckPostedBodyLength("hello", "<!-- marker -->"); err != nil {
		t.Fatalf("CheckPostedBodyLength() error = %v", err)
	}
	oversized := strings.Repeat("x", MaxPostedBodyLength+1)
	if err := CheckPostedBodyLength(oversized, ""); err == nil {
		t.Fatalf("CheckPostedBodyLength() accepted oversized body")
	}
	if err := CheckFinalBodyLength(oversized); err == nil {
		t.Fatalf("CheckFinalBodyLength() accepted oversized body")
	}
}
