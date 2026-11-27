package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/runtime/composition"
)

func TestImageMaintenanceApplyRefusesBeforeAnyResourceAccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runImageMaintenance(context.Background(), []string{"--apply"}, func(string) string { t.Fatal("loaded configuration during refused apply"); return "" }, &stdout, &stderr, func(context.Context, config.Config, []string, int) (retention.NativeInventory, error) {
		t.Fatal("accessed resources during refused apply")
		return retention.NativeInventory{}, nil
	})
	if exit != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "apply refused") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}
func TestImageMaintenanceArgumentValidation(t *testing.T) {
	for _, args := range [][]string{{"prune"}, {"--limit", "0"}, {"--limit", "10001"}, {"--pin", "latest"}} {
		var stdout, stderr bytes.Buffer
		if exit := runImageMaintenance(context.Background(), args, func(string) string { return "" }, &stdout, &stderr, nil); exit != 2 {
			t.Fatalf("%v exit=%d", args, exit)
		}
	}
}

func TestImageMaintenanceReportsIncompleteInventoryInBothFormats(t *testing.T) {
	for _, args := range [][]string{{}, {"--json", "--limit", "2"}} {
		var stdout, stderr bytes.Buffer
		getenv := func(name string) string {
			if name == "OMNIGREX_EXECUTION_BACKEND" {
				return "microsandbox"
			}
			return doctorTestEnvironment(name)
		}
		exit := runImageMaintenance(context.Background(), args, getenv, &stdout, &stderr, func(ctx context.Context, settings config.Config, pins []string, limit int) (retention.NativeInventory, error) {
			return retention.NativeImageInventory(ctx, composition.StateBinding(settings), nil, []string{settings.PiImage}, limit, false)
		})
		if exit != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "unsupported") {
			t.Fatalf("exit=%d out=%s err=%s", exit, &stdout, &stderr)
		}
		if len(args) > 0 && (!strings.Contains(stdout.String(), `"complete":false`) || !strings.Contains(stdout.String(), `"apply_supported":false`)) {
			t.Fatal(stdout.String())
		}
	}
}
