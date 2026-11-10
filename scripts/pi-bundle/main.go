// pi-bundle exposes the checked-in production selection to qualification scripts.
package main

import (
	"encoding/json"
	"fmt"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/environment"
	"os"
)

func main() {
	b, err := piresources.SelectedBundle("linux", "amd64")
	if err == nil && len(os.Args) == 2 {
		switch os.Args[1] {
		case "image":
			fmt.Println(b.Image)
			return
		case "candidate-amd64":
			fmt.Println(string(piresources.CandidateContract("amd64")))
			return
		case "candidate-arm64":
			fmt.Println(string(piresources.CandidateContract("arm64")))
			return
		case "record":
			err = json.NewEncoder(os.Stdout).Encode(b)
		case "qualification":
			var script string
			script, err = environment.QualificationScript("amd64", "pi-rpc", b.Resources)
			if err == nil {
				fmt.Print(script)
			}
		default:
			err = fmt.Errorf("unknown command")
		}
	} else if err == nil {
		err = fmt.Errorf("usage: pi-bundle image|record|qualification|candidate-amd64|candidate-arm64")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
