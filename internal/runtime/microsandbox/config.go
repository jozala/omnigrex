// Package microsandbox implements local Linux/KVM execution through the pinned Go SDK.
package microsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

const Version = "0.7.11"

// Options are trusted deployment settings, never repository configuration.
type Options struct {
	Executable, Firmware, Home, Namespace string
	WorkspaceRoot, StateRoot, ToolRoot    string
	CPUs                                  int
	MaximumDuration                       time.Duration
	RootDiskMiB                           int64
	MCPEndpoint                           string
}

func (options Options) Validate() error {
	if options.MCPEndpoint != "" {
		if _, err := mcpRule(options.MCPEndpoint); err != nil {
			return err
		}
	}
	for _, value := range []string{options.Executable, options.Firmware, options.Home, options.WorkspaceRoot, options.StateRoot, options.ToolRoot} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, ":\x00\n") {
			return execution.ErrInvalidSpec
		}
	}
	roots := []string{options.Home, options.WorkspaceRoot, options.StateRoot, options.ToolRoot}
	for index, first := range roots {
		for _, second := range roots[index+1:] {
			if first == second || strings.HasPrefix(first, second+"/") || strings.HasPrefix(second, first+"/") {
				return execution.ErrInvalidSpec
			}
		}
	}
	if len(options.Home) > 40 || strings.TrimSpace(options.Namespace) == "" || strings.ContainsAny(options.Namespace, "\x00\n=") || options.CPUs < 1 || options.CPUs > 64 || options.RootDiskMiB < 1024 || options.RootDiskMiB > 1<<20 || options.MaximumDuration < time.Minute {
		return execution.ErrInvalidSpec
	}
	return nil
}

type commandCLI struct{ options Options }

func (runner commandCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, runner.options.Executable, args...)
	// Inherit deployment networking/CA configuration, but not agent credentials.
	command.Env = append(os.Environ(), "MSB_HOME="+runner.options.Home, "NO_COLOR=1")
	command.WaitDelay = 2 * time.Second
	return command
}

func (runner commandCLI) Run(ctx context.Context, input io.Reader, output io.Writer, args ...string) error {
	command := runner.command(ctx, args...)
	command.Stdin, command.Stdout, command.Stderr = input, output, io.Discard
	if err := command.Run(); err != nil {
		// Neither CLI output nor secret-bearing arguments enter error objects.
		return errors.Join(ctx.Err(), fmt.Errorf("Microsandbox %s failed", args[0]))
	}
	return nil
}

func Check(ctx context.Context, options Options) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("%w: Microsandbox requires Linux/amd64 with KVM", execution.ErrUnsupportedRequirement)
	}
	if os.Geteuid() != 10001 || os.Getegid() != 10001 {
		return errors.New("Microsandbox POSIX bindings require control-plane UID/GID 10001")
	}
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return errors.New("Microsandbox requires usable /dev/kvm access")
	}
	if err := device.Close(); err != nil {
		return err
	}
	runner := commandCLI{options}
	var version strings.Builder
	if err := runner.Run(ctx, nil, &version, "--version"); err != nil {
		return err
	}
	if strings.TrimSpace(version.String()) != "msb "+Version {
		return errors.New("Microsandbox executable version must be 0.7.11")
	}
	return runner.Run(ctx, nil, io.Discard, "doctor")
}
