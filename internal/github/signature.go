package github

import (
	"errors"
	"strings"
)

// ErrPostedBodyTooLong reports a complete posted body that exceeds the GitHub limit.
var ErrPostedBodyTooLong = errors.New("comment body with signature exceeds GitHub limit")

// SignatureFooterPrefix introduces the visible Agent Participant signature.
const SignatureFooterPrefix = "_By Omnigrex:"

// MaxPostedBodyLength bounds the complete posted GitHub body, including the
// visible signature footer and any hidden operation marker.
const MaxPostedBodyLength = 65536

// RenderSignature returns the visible footer identifying the Agent Profile.
// The profile name is rendered verbatim in code spans; the Role display name
// is escaped for safe Markdown rendering.
func RenderSignature(profileName, roleDisplayName string) string {
	return "_By Omnigrex: `" + profileName + "` [" + EscapeSignatureDisplayName(roleDisplayName) + "]_"
}

// EscapeSignatureDisplayName escapes Markdown-significant characters in a Role display name.
func EscapeSignatureDisplayName(display string) string {
	var rendered strings.Builder
	for _, character := range display {
		switch character {
		case '\\', '`', '*', '_', '[', ']', '(', ')', '#', '+', '-', '.', '!', '|', '{', '}', '<', '>', '~':
			rendered.WriteRune('\\')
			rendered.WriteRune(character)
		default:
			rendered.WriteRune(character)
		}
	}
	return rendered.String()
}

// AppendSignature appends the visible footer to an agent body.
// It is idempotent: a body already ending with the exact signature is unchanged.
func AppendSignature(body, signature string) string {
	if signature == "" {
		return body
	}
	trimmed := strings.TrimRight(body, "\n")
	if strings.HasSuffix(trimmed, signature) {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return signature
	}
	return trimmed + "\n\n" + signature
}

// JoinPostedBody assembles the exact posted body from the signed visible text
// and the hidden operation marker, mirroring API body assembly.
func JoinPostedBody(signedBody, marker string) string {
	parts := make([]string, 0, 2)
	for _, part := range []string{signedBody, marker} {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

// CheckPostedBodyLength rejects an oversized complete body before posting
// without truncating agent text.
func CheckPostedBodyLength(signedBody, marker string) error {
	if len(JoinPostedBody(signedBody, marker)) > MaxPostedBodyLength {
		return ErrPostedBodyTooLong
	}
	return nil
}

// CheckFinalBodyLength rejects an oversized final posted body, used when the
// body already contains its hidden marker.
func CheckFinalBodyLength(final string) error {
	if len(final) > MaxPostedBodyLength {
		return ErrPostedBodyTooLong
	}
	return nil
}

// StripSignature removes one trailing signature footer, reporting whether one was present.
func StripSignature(body string) (string, bool) {
	trimmed := strings.TrimRight(body, "\n")
	index := strings.LastIndex(trimmed, "\n\n"+SignatureFooterPrefix)
	if index < 0 {
		if strings.HasPrefix(trimmed, SignatureFooterPrefix) {
			return "", true
		}
		return body, false
	}
	candidate := trimmed[index+2:]
	if !strings.HasPrefix(candidate, SignatureFooterPrefix) || !strings.HasSuffix(candidate, "_") {
		return body, false
	}
	return strings.TrimRight(trimmed[:index], "\n"), true
}

// MatchesSignedBody reports whether a published body without its hidden marker
// corresponds to the agent body with either a valid signature footer or,
// for pre-deployment publications, no footer at all.
func MatchesSignedBody(publishedWithoutMarker, agentBody string) bool {
	published := strings.TrimSpace(publishedWithoutMarker)
	agent := strings.TrimSpace(agentBody)
	if published == agent {
		return true
	}
	stripped, ok := StripSignature(published)
	if !ok {
		return false
	}
	return strings.TrimSpace(stripped) == agent
}
