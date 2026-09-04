package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

var (
	ErrCredentialInRemote = errors.New("repository remote must not contain credentials")
	ErrInvalidCredential  = errors.New("invalid repository credential")
	ErrInvalidRemote      = errors.New("invalid repository remote")
	ErrInvalidRevision    = errors.New("invalid Git revision")
)

type Checkout struct {
	AssignmentID  string
	RepositoryURL string
	Credential    string
	Revision      string
}

func (lifecycle *Lifecycle) PrepareWorkspace(ctx context.Context, checkout Checkout) (paths Paths, err error) {
	paths, err = lifecycle.Paths(checkout.AssignmentID)
	if err != nil {
		return Paths{}, err
	}
	if err := validateRemote(checkout.RepositoryURL); err != nil {
		return Paths{}, err
	}
	if err := validateCredential(checkout.Credential); err != nil {
		return Paths{}, err
	}
	revision, err := normalizeObjectID(checkout.Revision)
	if err != nil {
		return Paths{}, err
	}
	if err := ensureAssignmentDirectory(lifecycle.workspaceRoot, filepath.Dir(paths.Workspace)); err != nil {
		return Paths{}, fmt.Errorf("create assignment workspace parent: %w", err)
	}
	if _, err := inspectOwnedDirectory(paths.Workspace); err != nil {
		return Paths{}, err
	}
	if err := os.RemoveAll(paths.Workspace); err != nil {
		return Paths{}, fmt.Errorf("replace assignment workspace: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(paths.Workspace)
		}
	}()

	if err = lifecycle.git(ctx, "clone repository", "", checkout.Credential,
		"clone", "--no-checkout", "--origin=origin", "--config", "core.hooksPath=/dev/null", "--", checkout.RepositoryURL, paths.Workspace); err != nil {
		return Paths{}, err
	}
	if err = lifecycle.git(ctx, "set credential-free remote", paths.Workspace, "",
		"remote", "set-url", "origin", checkout.RepositoryURL); err != nil {
		return Paths{}, err
	}
	if err = lifecycle.git(ctx, "disable repository hooks", paths.Workspace, "",
		"config", "--local", "core.hooksPath", "/dev/null"); err != nil {
		return Paths{}, err
	}
	if err = lifecycle.git(ctx, "fetch exact revision", paths.Workspace, checkout.Credential,
		"fetch", "--force", "--no-tags", "origin", revision); err != nil {
		return Paths{}, err
	}
	if err = lifecycle.git(ctx, "check out exact revision", paths.Workspace, "",
		"checkout", "--detach", "--force", revision); err != nil {
		return Paths{}, err
	}
	if err = lifecycle.git(ctx, "reset exact revision", paths.Workspace, "",
		"reset", "--hard", revision); err != nil {
		return Paths{}, err
	}
	if err = lifecycle.git(ctx, "clean workspace", paths.Workspace, "",
		"clean", "-ffdx"); err != nil {
		return Paths{}, err
	}
	if _, err = inspectOwnedDirectory(paths.Workspace); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

func (lifecycle *Lifecycle) git(ctx context.Context, operation, directory, credential string, arguments ...string) error {
	_, err := lifecycle.gitOutput(ctx, operation, directory, credential, nil, arguments...)
	return err
}

func (lifecycle *Lifecycle) gitOutput(ctx context.Context, operation, directory, credential string, extraEnvironment map[string]string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, lifecycle.gitExecutable, arguments...)
	command.Dir = directory
	command.Env = commandEnvironment(credential, "")
	for name, value := range extraEnvironment {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(output)), nil
	}
	detail := strings.TrimSpace(redact(string(output), credential))
	if detail == "" {
		detail = redact(err.Error(), credential)
	}
	return "", fmt.Errorf("%s: %s", operation, detail)
}

func commandEnvironment(credential, miseDataDir string) []string {
	allowed := []string{
		"ALL_PROXY", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "PATH", "SSL_CERT_DIR", "SSL_CERT_FILE", "TMP", "TEMP", "TMPDIR",
		"all_proxy", "https_proxy", "http_proxy", "no_proxy",
	}
	environment := make([]string, 0, len(allowed)+12)
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	environment = append(environment,
		"HOME=",
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0="+os.DevNull,
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
	)
	configCount := 2
	if credential != "" {
		environment = append(environment,
			"GIT_CONFIG_KEY_2=http.extraHeader",
			"GIT_CONFIG_VALUE_2=Authorization: Bearer "+credential,
		)
		configCount++
	}
	environment = append(environment, fmt.Sprintf("GIT_CONFIG_COUNT=%d", configCount))
	if miseDataDir != "" {
		environment = append(environment, "MISE_DATA_DIR="+miseDataDir)
	}
	return environment
}

func validateRemote(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
		return ErrInvalidRemote
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ErrInvalidRemote
	}
	if parsed.User != nil {
		return ErrCredentialInRemote
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrInvalidRemote
	}
	if parsed.Scheme == "" {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return ErrInvalidRemote
		}
		return nil
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" && parsed.Scheme != "file" {
		return ErrInvalidRemote
	}
	if parsed.Scheme != "file" && parsed.Host == "" {
		return ErrInvalidRemote
	}
	return nil
}

func validateCredential(value string) error {
	for _, character := range value {
		if character > unicode.MaxASCII || unicode.IsSpace(character) || unicode.IsControl(character) {
			return ErrInvalidCredential
		}
	}
	return nil
}

func normalizeObjectID(value string) (string, error) {
	if len(value) != 40 && len(value) != 64 {
		return "", ErrInvalidRevision
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return "", ErrInvalidRevision
			}
		}
	}
	return strings.ToLower(value), nil
}

func redact(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}
