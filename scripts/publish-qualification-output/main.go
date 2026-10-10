// This source-checkout helper publishes completed qualification outputs.
// It is not part of the clone-free deployment bundle.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/dirstate"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: publish-qualification-output compatibility|state INPUT OUTPUT")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := publish(ctx, os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "qualification output validation or exclusive publication failed")
		os.Exit(1)
	}
}

func publish(ctx context.Context, kind, inputPath, outputPath string) (err error) {
	if kind != "compatibility" && kind != "state" {
		return errors.New("unsupported output kind")
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	const limit = 384 << 20
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return errors.New("invalid qualification output")
	}
	// Stage on the destination filesystem. os.Link provides one atomic,
	// exclusive publication on both Linux and macOS, including directory and
	// symlink destinations, with no partial final path or overwrite window.
	staged, err := os.CreateTemp(filepath.Dir(outputPath), ".pi-qualification-output-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, staged.Close(), os.Remove(staged.Name())) }()
	count, err := io.Copy(staged, io.LimitReader(input, limit+1))
	if err != nil {
		return err
	}
	if count != info.Size() || count > limit {
		return errors.New("incomplete qualification output")
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if kind == "compatibility" {
		if _, err := profile.DecodeCompatibilityResultsFile(staged); err != nil {
			return err
		}
	} else if err := dirstate.ValidateArchive(ctx, staged); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(staged.Name(), outputPath); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(outputPath))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
