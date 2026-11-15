package webhook

import (
	"strings"
	"testing"
)

func TestCollaborationCommandRecognition(t *testing.T) {
	for _, body := range []string{"ordinary", "> /omnigrex answer R1\nQuoted", "```\n/omnigrex steer\n```", "    /omnigrex steer\nCode", "\t/omnigrex steer\nCode", "text\n/omnigrex steer\nLater"} {
		if _, ok := ParseCollaborationCommand("issue_comment", "created", body); ok {
			t.Errorf("executed %q", body)
		}
	}
	for _, action := range []string{"edited", "deleted"} {
		if _, ok := ParseCollaborationCommand("issue_comment", action, "/omnigrex steer\nChanged"); ok {
			t.Fatal(action)
		}
	}
	if _, ok := ParseCollaborationCommand("reaction", "created", "/omnigrex steer\nChanged"); ok {
		t.Fatal("reaction executed")
	}
	command, ok := ParseCollaborationCommand("issue_comment", "created", "\n\n/omnigrex answer R7\n1. Keep it.\n2. Cannot decide.\n/omnigrex steer\nquoted content")
	if !ok || command.Kind != "answer" || command.Target != "R7" || command.Body != "1. Keep it.\n2. Cannot decide.\n/omnigrex steer\nquoted content" {
		t.Fatalf("%+v %t", command, ok)
	}
	command, ok = ParseCollaborationCommand("issue_comment", "created", "/omnigrex answer R7 /omnigrex steer\nTwo commands")
	if !ok || command.Kind != "invalid" {
		t.Fatal("multiple first-line commands admitted")
	}
}

func TestImplementationCommandsRequireExactUnqualifiedConsent(t *testing.T) {
	for _, kind := range []string{"implement", "withdraw-implementation"} {
		for _, body := range []string{"/omnigrex " + kind + " P2", "\n/omnigrex " + kind + " P2\n \n"} {
			c, ok := ParseCollaborationCommand("issue_comment", "created", body)
			if !ok || c.Kind != kind || c.Target != "P2" || strings.TrimSpace(c.Body) != "" {
				t.Fatal(c, ok)
			}
		}
		for _, body := range []string{"/omnigrex " + kind + " P2 But use PostgreSQL", "/omnigrex " + kind + " P2\nBut use PostgreSQL"} {
			c, ok := ParseCollaborationCommand("issue_comment", "created", body)
			if !ok || c.Kind != kind || strings.TrimSpace(c.Body) == "" {
				t.Fatal("conditional consent lost", c, ok)
			}
		}
		for _, body := range []string{"> /omnigrex " + kind + " P2", "yes\n/omnigrex " + kind + " P2", "    /omnigrex " + kind + " P2", "`/omnigrex " + kind + " P2`"} {
			if _, ok := ParseCollaborationCommand("issue_comment", "created", body); ok {
				t.Fatal("implicit consent recognized", body)
			}
		}
		for _, action := range []string{"edited", "deleted"} {
			if _, ok := ParseCollaborationCommand("issue_comment", action, "/omnigrex "+kind+" P2"); ok {
				t.Fatal("edit became authority")
			}
		}
	}
}
