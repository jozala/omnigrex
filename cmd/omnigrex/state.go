package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/uuidtext"
	"github.com/jozala/omnigrex/internal/workflow"
)

// state export/restore operates on an offline Participant Session-data artifact.
// Standard coordinated backups additionally retain PostgreSQL and captures.
func runStateCommand(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, lookup ...func(string) (string, bool)) int {
	flags := flag.NewFlagSet("state", flag.ContinueOnError)
	flags.SetOutput(stderr)
	participant := flags.String("participant", "", "exact Agent Participant UUID")
	if len(args) == 0 || args[0] != "export" && args[0] != "restore" {
		_, _ = fmt.Fprintln(stderr, "usage: omnigrex state export|restore --participant UUID")
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || !uuidtext.ValidCanonicalNonNil(*participant) {
		_, _ = fmt.Fprintln(stderr, "state requires one canonical --participant UUID")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// Signal/deadline cancellation must also interrupt a blocked pipe, not just
	// database calls. Stop the callback before normal cancellation on return.
	stopIO := context.AfterFunc(ctx, func() {
		if args[0] == "restore" {
			if input, ok := stdin.(io.Closer); ok {
				_ = input.Close()
			}
		}
		if args[0] == "export" {
			if output, ok := stdout.(io.Closer); ok {
				_ = output.Close()
			}
		}
	})
	defer stopIO()
	settings, err := config.Load(getenv, lookup...)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	policies := role.BuiltinPolicyCatalog()
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	reducer, err := workflow.NewReducer(definition, workflow.BuiltinInfrastructureRetryLimit)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	database, err := store.Open(ctx, settings.DatabaseURL, settings.DatabasePasswordSecretFile, store.Config{Reducer: reducer, Policies: policies})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer database.Close()
	// This also rejects a backend/namespace change before any physical operation.
	if err := database.BindExecutionBackend(ctx, composition.StateBinding(settings)); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	err = database.WithStateRecovery(ctx, *participant, func(ref execution.StateRef, source execution.StateMetadataSource) error {
		recovery, err := composition.StateRecovery(ctx, settings, source)
		if err != nil {
			return err
		}
		if args[0] == "export" {
			return recovery.Export(ctx, ref, stdout)
		}
		return recovery.Restore(ctx, ref, stdin)
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
