package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/retention"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type imagePins []string

func (p *imagePins) String() string { return "exact image references" }
func (p *imagePins) Set(value string) error {
	if !profile.IsExactRegistryImage(value) || len(*p) >= 1000 {
		return errors.New("pin requires an exact registry image; maximum 1000 pins")
	}
	*p = append(*p, value)
	return nil
}

type imageInventoryExecutor func(context.Context, config.Config, []string, int) (retention.NativeInventory, error)

func runImageMaintenance(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, execute imageInventoryExecutor, lookup ...func(string) (string, bool)) int {
	flags := flag.NewFlagSet("image-maintenance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apply := flags.Bool("apply", false, "refuse unless native GC is qualified (currently unsupported)")
	jsonOutput := flags.Bool("json", false, "emit machine-readable inventory")
	limit := flags.Int("limit", 1000, "maximum image protection roots (1-10000)")
	var pins imagePins
	flags.Var(&pins, "pin", "additional exact provisioned/pinned image; repeatable")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *limit < 1 || *limit > 10000 {
		return 2
	}
	// Refuse before constructing any backend or opening storage. A prior dry-run
	// cannot authorize apply, and no filesystem cache deletion fallback exists.
	if *apply {
		fmt.Fprintln(stderr, retention.ErrNativeApplyUnsupported)
		return 1
	}
	settings, err := config.Load(getenv, lookup...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if settings.ExecutionBackend != "microsandbox" {
		fmt.Fprintln(stderr, "image-maintenance requires the Microsandbox backend")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report, err := execute(ctx, settings, pins, *limit)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return 1
		}
	} else {
		fmt.Fprintln(stdout, "Native image inventory: incomplete; apply unsupported; no runtime artifacts mutated")
		for _, root := range report.Roots {
			fmt.Fprintf(stdout, "%s %s: %s\n", root.Status, root.Image, root.Reason)
		}
		for _, dimension := range report.Dimensions {
			fmt.Fprintf(stdout, "%s %s: %s\n", dimension.Status, dimension.Category, dimension.Reason)
		}
		fmt.Fprintln(stdout, "Dependency:", report.Dependency)
	}
	return 0
}
func productionImageInventory(ctx context.Context, settings config.Config, pins []string, limit int) (retention.NativeInventory, error) {
	definition, err := workflow.NewBuiltinDefinition(role.BuiltinCatalog())
	if err != nil {
		return retention.NativeInventory{}, err
	}
	reducer, err := workflow.NewReducer(definition, workflow.BuiltinInfrastructureRetryLimit)
	if err != nil {
		return retention.NativeInventory{}, err
	}
	database, err := store.Open(ctx, settings.DatabaseURL, settings.DatabasePasswordSecretFile, store.Config{Reducer: reducer, Policies: role.BuiltinPolicyCatalog()})
	if err != nil {
		return retention.NativeInventory{}, err
	}
	defer database.Close()
	binding := composition.StateBinding(settings)
	refs, truncated, err := database.ImageMaintenanceRoots(ctx, binding, limit)
	if err != nil {
		return retention.NativeInventory{}, err
	}
	images := append([]string{settings.PiImage}, pins...)
	return retention.NativeImageInventory(ctx, binding, refs, images, limit, truncated)
}
