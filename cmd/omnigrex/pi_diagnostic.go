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
	"github.com/jozala/omnigrex/internal/runtime/agent"
	"github.com/jozala/omnigrex/internal/runtime/composition"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/pi"
)

func runPiDiagnostic(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pi-diagnostic", flag.ContinueOnError)
	flags.SetOutput(stderr)
	image := flags.String("image", "", "digest-pinned disposable Pi diagnostic image")
	backend := flags.String("backend", "docker", "execution backend: docker or microsandbox")
	arch := flags.String("arch", "amd64", "Linux image architecture: amd64 or arm64")
	configuration := flags.Bool("configuration", false, "qualify owned Pi configuration and authenticated fixture MCP")
	reviewer := flags.Bool("reviewer", false, "qualify Reviewer tool restrictions (requires --configuration)")
	namespace := flags.String("namespace", "", "installation diagnostic namespace (defaults to the backend's installation namespace)")
	home := flags.String("microsandbox-home", "/tmp/msb-pi", "private Microsandbox diagnostic inventory/cache")
	executable := flags.String("microsandbox-executable", "/usr/local/bin/msb", "pinned Microsandbox executable")
	firmware := flags.String("microsandbox-firmware", "/usr/local/lib/libkrunfw.so.5.6.1", "pinned Microsandbox firmware")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	spec := pi.DiagnosticSpec(*image, execution.Platform{OS: "linux", Architecture: *arch})
	if *configuration {
		spec = pi.ConfigurationDiagnosticSpec(*image, execution.Platform{OS: "linux", Architecture: *arch})
	}
	if *namespace == "" {
		if *backend == "microsandbox" {
			*namespace = "omnigrex"
		} else {
			*namespace = "omnigrex-runtime-state"
		}
	}
	spec.Namespace = *namespace
	if flags.NArg() != 0 || *reviewer && !*configuration || spec.Validate() != nil || (*backend != "docker" && *backend != "microsandbox") {
		_, _ = fmt.Fprintln(stderr, "invalid Pi diagnostic arguments; require an exact image and supported backend/platform")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	settings := config.Config{ExecutionBackend: *backend, RuntimeStateVolume: *namespace, MicrosandboxHome: *home, MicrosandboxExecutable: *executable, MicrosandboxFirmware: *firmware, MicrosandboxNamespace: *namespace, MicrosandboxCPUs: 1, MicrosandboxRootDiskMiB: 4096, WorkspaceRoot: "/tmp/pi-diagnostic-workspaces", RuntimeStateRoot: "/tmp/pi-diagnostic-states", MiseRoot: "/tmp/pi-diagnostic-tools", AgentTurnExecutionTurnTimeout: time.Minute}
	process, owner, err := composition.StartProtocolDiagnostic(ctx, spec, composition.ProbeOptions{}, io.Discard, settings)
	if err != nil {
		_, _ = fmt.Fprintln(stdout, "FAIL Pi diagnostic launch")
		return 1
	}
	var connection agent.Connection
	if *configuration {
		roleID := role.Developer
		if *reviewer {
			roleID = role.Reviewer
		}
		inputs, inputErr := pi.ConfigurationDiagnostic(roleID)
		if inputErr != nil {
			err = inputErr
		} else {
			connection, err = pi.Start(ctx, process.Transport(), inputs, pi.Options{ExpectedSessionID: "omnigrex-pi-configuration"})
		}
		if err == nil {
			err = pi.CheckConfigurationDiagnostic(ctx, connection)
		}
	} else {
		connection = pi.Connect(process.Transport(), pi.Options{ExpectedSessionID: "omnigrex-pi-diagnostic"})
		err = pi.CheckDiagnostic(ctx, connection)
	}
	var closeErr error
	if connection != nil {
		closeErr = connection.Close()
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer stop()
	stopErr := process.Stop(cleanup, 5*time.Second)
	var removeErr error
	if stopErr == nil {
		removeErr = process.Remove(cleanup)
	}
	ownerErr := owner.Close()
	if errors.Join(err, closeErr, stopErr, removeErr, ownerErr) != nil {
		_, _ = fmt.Fprintln(stdout, "FAIL Pi protocol or disposable environment cleanup")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "PASS Pi correlated RPC, prompt settlement, and disposable cleanup")
	return 0
}
