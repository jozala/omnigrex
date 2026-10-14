package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestStateCommandFailuresNeverContaminateArchiveOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		exit int
	}{
		{"missing operation", nil, 2},
		{"unknown operation", []string{"migrate"}, 2},
		{"invalid owner", []string{"export", "--participant", "../other-participant"}, 2},
		{"unavailable deployment", []string{"export", "--participant", "10000000-0000-4000-8000-000000000001"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := runStateCommand(context.Background(), test.args, func(string) string { return "" }, strings.NewReader(""), &stdout, &stderr)
			if exit != test.exit || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("exit=%d archive=%q diagnostic=%q", exit, stdout.String(), stderr.String())
			}
		})
	}
}
