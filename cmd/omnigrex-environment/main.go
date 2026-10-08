// omnigrex-environment runs only inside the repository Execution Environment.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jozala/omnigrex/internal/runtime/environment"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "contract-version" {
		fmt.Println("omnigrex-environment/v1")
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "invalid environment bootstrap command")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	values, err := environment.Bootstrap(ctx, "/workspace", environment.DataDirectory, environment.MiseExecutable, os.Environ(), os.Stderr)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = syscall.Exec(environment.RuntimeExecutable, append([]string{environment.RuntimeExecutable}, os.Args[2:]...), values)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
