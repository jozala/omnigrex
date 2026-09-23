package doctor

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Check struct {
	Name string
	Run  func(context.Context) error
}

type Result struct {
	Name string
	Err  error
}

type Repository struct {
	Owner string
	Name  string
}

func ParseRepository(value string) (Repository, error) {
	owner, name, found := strings.Cut(value, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") ||
		strings.TrimSpace(owner) != owner || strings.TrimSpace(name) != name {
		return Repository{}, errors.New("repository must use OWNER/REPOSITORY syntax")
	}
	return Repository{Owner: owner, Name: name}, nil
}

type Runner struct {
	timeout time.Duration
	checks  []Check
}

func NewRunner(timeout time.Duration, checks []Check) (*Runner, error) {
	if timeout <= 0 || len(checks) == 0 {
		return nil, errors.New("doctor requires a positive timeout and at least one check")
	}
	seen := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		if strings.TrimSpace(check.Name) == "" || check.Run == nil {
			return nil, errors.New("doctor check requires a name and implementation")
		}
		if _, duplicate := seen[check.Name]; duplicate {
			return nil, errors.New("doctor check names must be unique")
		}
		seen[check.Name] = struct{}{}
	}
	return &Runner{timeout: timeout, checks: append([]Check(nil), checks...)}, nil
}

func (runner *Runner) Run(ctx context.Context) []Result {
	results := make([]Result, 0, len(runner.checks))
	for _, check := range runner.checks {
		checkCtx, cancel := context.WithTimeout(ctx, runner.timeout)
		err := check.Run(checkCtx)
		cancel()
		results = append(results, Result{Name: check.Name, Err: err})
	}
	return results
}
