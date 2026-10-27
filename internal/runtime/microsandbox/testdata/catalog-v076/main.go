// A separate pinned module is deliberate: never load two FFI versions into one
// process. Used only against disposable catalog copies, never deployment state.
package main

import (
	"context"
	"fmt"
	sdk "github.com/superradcompany/microsandbox/sdk/go"
	"os"
	"path/filepath"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if len(os.Args) != 2 {
		panic("expected seed or verify")
	}
	const name = "retained-v076"
	if os.Args[1] == "seed" {
		volume, err := sdk.CreateVolume(ctx, name)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(volume.Path(), "retained.txt"), []byte("acknowledged-v076"), 0600); err != nil {
			panic(err)
		}
	} else if os.Args[1] != "verify" {
		panic("invalid mode")
	}
	volume, err := sdk.GetVolume(ctx, name)
	if err != nil {
		panic(err)
	}
	data, err := os.ReadFile(filepath.Join(volume.Path(), "retained.txt"))
	if err != nil || string(data) != "acknowledged-v076" {
		panic("retained data differs")
	}
	fmt.Println("V076 CATALOG PASS")
}
