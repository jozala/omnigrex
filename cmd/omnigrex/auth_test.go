package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/providerauth"
)

func TestAuthCommandsDoNotRequireGitHubOrOpenAIAPIKeys(t *testing.T) {
	dir := t.TempDir()
	getenv := func(name string) string {
		switch name {
		case "OMNIGREX_OPENAI_AUTH_MODE":
			return "subscription"
		case "OMNIGREX_PROVIDER_AUTH_ROOT":
			return dir
		default:
			return ""
		}
	}
	var output, diagnostic bytes.Buffer
	if code := runAuthCommand(context.Background(), []string{"status"}, getenv, strings.NewReader(""), &output, &diagnostic); code != 0 {
		t.Fatalf("status exit = %d: %s", code, diagnostic.String())
	}
	var status providerauth.Status
	if json.Unmarshal(output.Bytes(), &status) != nil || status.Mode != "subscription" || status.SignedIn || !status.AuthBlocked {
		t.Fatal("unexpected unauthenticated status")
	}
	output.Reset()
	if code := runAuthCommand(context.Background(), []string{"login", "--new-account"}, getenv, strings.NewReader("invalid-callback\n"), &output, &diagnostic); code != 1 {
		t.Fatal("invalid callback was accepted")
	}
	if !strings.Contains(output.String(), "agent_name_hint=Omnigrex") || strings.Contains(diagnostic.String(), "invalid-callback") {
		t.Fatal("missing branded sign-in or echoed callback")
	}
	if code := runAuthCommand(context.Background(), []string{"logout"}, getenv, strings.NewReader(""), &output, &diagnostic); code != 0 {
		t.Fatal("offline logout failed")
	}
}

func TestAuthModeAndArgumentsNeverSilentlyChooseSubscriptionOrPaidFallback(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"login", "--unknown"}, {"status", "extra"}} {
		if code := runAuthCommand(context.Background(), args, func(string) string { return "" }, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Fatalf("invalid command %v exit = %d", args, code)
		}
	}
	var output bytes.Buffer
	if code := runAuthCommand(context.Background(), []string{"status"}, func(string) string { return "" }, strings.NewReader(""), &output, &bytes.Buffer{}); code != 0 || !strings.Contains(output.String(), `"mode":"api_key"`) {
		t.Fatal("default authentication mode changed")
	}
	if code := runAuthCommand(context.Background(), []string{"login"}, func(string) string { return "" }, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatal("subscription login ignored the explicit API-key mode")
	}
}
