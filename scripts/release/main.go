// Release preparation and designation only; this command never builds images or deploys.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func identityFromEnv() (Build, error) {
	run, e := strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	if e != nil {
		return Build{}, errors.New("GITHUB_RUN_ID must identify the exact CI run")
	}
	attempt, e := strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if e != nil {
		return Build{}, errors.New("GITHUB_RUN_ATTEMPT is required")
	}
	b := Build{SchemaVersion: 1, Repository: os.Getenv("GITHUB_REPOSITORY"), Commit: os.Getenv("GITHUB_SHA"), RunID: run, Attempt: attempt, Platform: "linux/amd64", Images: map[string]string{}}
	for _, n := range products {
		b.Images[n] = os.Getenv(strings.ToUpper(strings.ReplaceAll(n, "-", "_")) + "_IMAGE")
	}
	return b, nil
}
func execute(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: release pack|verify|ready|alpha|stable OUTPUT_DIRECTORY")
	}
	command, dir := args[0], args[1]
	if command == "pack" {
		b, e := identityFromEnv()
		if e != nil {
			return e
		}
		return pack(dir, ".", b)
	}
	if command == "verify" {
		_, _, e := verify(dir)
		return e
	}
	if command != "ready" && command != "alpha" && command != "stable" {
		return errors.New("unknown release command")
	}
	workflowToken := os.Getenv("GH_TOKEN")
	if workflowToken == "" {
		return errors.New("GH_TOKEN workflow token is required")
	}
	apiForToken := func(token string) githubAPI {
		return httpAPI{token: token, baseURL: "https://api.github.com", client: releaseHTTPClient()}
	}
	api := apiForToken(workflowToken)
	if command == "ready" {
		b, e := identityFromEnv()
		if e != nil {
			return e
		}
		return ready(ctx, api, dir, b)
	}
	if os.Getenv("GITHUB_REPOSITORY") != repository || os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" {
		return errors.New("release must be manually dispatched on main in " + repository)
	}
	tagger := appTagger{appID: os.Getenv("OMNIGREX_RELEASE_APP_ID"), privateKey: os.Getenv("OMNIGREX_RELEASE_APP_PRIVATE_KEY"), apiForToken: apiForToken, now: time.Now}
	// Check configuration before downloading artifacts or creating partial state.
	if _, e := appJWT(tagger.appID, tagger.privateKey, time.Now()); e != nil {
		return e
	}
	p := publication{api: api, createTag: tagger.create, upload: func(ctx context.Context, tag string, files []string) error {
		return ghUpload(ctx, workflowToken, tag, files)
	}}
	return p.publish(ctx, publishInput{Channel: command, Version: os.Getenv("VERSION"), Notes: os.Getenv("NOTES"), Breaking: os.Getenv("BREAKING"), Upgrades: os.Getenv("UPGRADES"), SourceRun: os.Getenv("SOURCE_RUN"), Alpha: os.Getenv("ALPHA")}, dir)
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if e := execute(ctx, os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "release failed:", e)
		os.Exit(1)
	}
}
