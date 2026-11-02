package webhook

import "testing"

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
