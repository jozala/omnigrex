//go:build integration

package pi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestRPCClientCorrelatesConcurrentResponses(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestRPCFixtureProcess$")
	command.Env = append(os.Environ(), "OMNIGREX_RPC_HELPER=1")
	client, err := startRPC(command)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.close(); err != nil {
			t.Error(err)
		}
	}()
	var group sync.WaitGroup
	for index := range 4 {
		group.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			kind := fmt.Sprintf("command-%d", index)
			response, err := client.request(ctx, kind, nil)
			if err != nil || response["command"] != kind {
				t.Errorf("%s: %v", kind, err)
			}
		})
	}
	group.Wait()
}

func TestRPCFixtureProcess(t *testing.T) {
	if os.Getenv("OMNIGREX_RPC_HELPER") != "1" {
		t.Skip("subprocess fixture")
	}
	scanner := bufio.NewScanner(os.Stdin)
	var commands []map[string]any
	for range 4 {
		if !scanner.Scan() {
			os.Exit(2)
		}
		var command map[string]any
		if json.Unmarshal(scanner.Bytes(), &command) != nil {
			os.Exit(2)
		}
		commands = append(commands, command)
	}
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		if json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true}) != nil {
			os.Exit(2)
		}
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
