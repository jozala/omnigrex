package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/providerauth"
)

func runAuthCommand(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: omnigrex auth login [--new-account] | status | logout | unblock")
		return 2
	}
	flags := flag.NewFlagSet("auth "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	var newAccount *bool
	if args[0] == "login" {
		newAccount = flags.Bool("new-account", false, "choose a different ChatGPT account or workspace")
	}
	switch args[0] {
	case "login", "status", "logout", "unblock":
	default:
		_, _ = fmt.Fprintln(stderr, "unknown authentication command")
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "authentication commands do not accept positional arguments")
		return 2
	}
	mode, directory, err := config.LoadProviderAuth(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if mode != "subscription" {
		if args[0] == "status" {
			_ = json.NewEncoder(stdout).Encode(struct {
				Mode string `json:"mode"`
			}{Mode: mode})
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "set OMNIGREX_OPENAI_AUTH_MODE=subscription and recreate the controller before managing ChatGPT authentication")
		return 1
	}
	client, closeClient, err := providerauth.OpenOperator(ctx, directory)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer closeClient()
	switch args[0] {
	case "status":
		status, statusErr := client.Status(ctx)
		err = statusErr
		if err == nil {
			err = json.NewEncoder(stdout).Encode(status)
		}
	case "login":
		err = loginChatGPT(ctx, client, *newAccount, stdin, stdout)
	case "logout":
		var result providerauth.LogoutResult
		result, err = client.Logout(ctx)
		if err == nil {
			_, _ = fmt.Fprintln(stdout, "ChatGPT login removed locally; new token issuance and renewal stopped.")
			if !result.RemoteRevocationConfirmed {
				_, _ = fmt.Fprintln(stdout, "Remote revocation was not confirmed. Disconnect Omnigrex in ChatGPT Settings if needed.")
			}
		}
	case "unblock":
		err = client.Unblock(ctx)
		if err == nil {
			_, _ = fmt.Fprintln(stdout, "Subscription quota block cleared for another attempt. Explicitly resume affected Work Items; restored capacity has not been verified.")
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func loginChatGPT(parent context.Context, client providerauth.Operator, newAccount bool, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("could not start ChatGPT loopback callback listener")
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/auth/callback"
	start, err := client.BeginLogin(ctx, providerauth.LoginRequest{RedirectURI: redirect, NewAccount: newAccount})
	if err != nil {
		return err
	}
	callbacks := make(chan string, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/auth/callback" || r.URL.Query().Get("state") != start.ID {
			http.NotFound(w, r)
			return
		}
		select {
		case callbacks <- redirect + "?" + r.URL.RawQuery:
		default:
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "Return to the Omnigrex terminal to check sign-in completion.\n")
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	_, _ = fmt.Fprintln(stdout, "Continue with ChatGPT. Open this URL in your browser:")
	_, _ = fmt.Fprintln(stdout, start.URL)
	_, _ = fmt.Fprintln(stdout, "If the deployment is remote and the callback cannot connect, paste the full final browser redirect URL here:")
	manual := make(chan string, 1)
	readError := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 4096), 128<<10)
		if scanner.Scan() {
			manual <- strings.TrimSpace(scanner.Text())
			return
		}
		readError <- errors.New("sign-in callback input ended")
	}()
	var callback string
	select {
	case <-ctx.Done():
		return errors.New("ChatGPT sign-in cancelled or timed out")
	case callback = <-callbacks:
	case callback = <-manual:
	case err := <-readError:
		return err
	}
	if err := client.CompleteLogin(ctx, providerauth.LoginCompletion{ID: start.ID, Callback: callback}); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "ChatGPT login saved. Subsequent model requests use this account, including existing Agent Sessions. Work Items in Human Handoff require explicit resumption.")
	return nil
}
