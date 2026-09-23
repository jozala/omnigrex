package doctor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/doctor"
)

func TestRunnerExecutesEveryCheckInOrder(t *testing.T) {
	wantFailure := errors.New("unavailable")
	order := make([]string, 0, 3)
	runner, err := doctor.NewRunner(time.Second, []doctor.Check{
		{Name: "configuration", Run: recordDoctorCheck(&order, "configuration", nil)},
		{Name: "postgres", Run: recordDoctorCheck(&order, "postgres", wantFailure)},
		{Name: "docker", Run: recordDoctorCheck(&order, "docker", nil)},
	})
	if err != nil {
		t.Fatal(err)
	}

	results := runner.Run(context.Background())
	if len(results) != 3 || results[0].Name != "configuration" || results[0].Err != nil ||
		results[1].Name != "postgres" || !errors.Is(results[1].Err, wantFailure) ||
		results[2].Name != "docker" || results[2].Err != nil {
		t.Fatalf("results = %#v", results)
	}
	if got := order; len(got) != 3 || got[0] != "configuration" || got[1] != "postgres" || got[2] != "docker" {
		t.Fatalf("check order = %v", got)
	}
}

func TestRunnerGivesEachCheckAnIndependentTimeout(t *testing.T) {
	runner, err := doctor.NewRunner(10*time.Millisecond, []doctor.Check{
		{Name: "blocked", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		{Name: "later", Run: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatalf("later check inherited expired context: %v", ctx.Err())
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	results := runner.Run(context.Background())
	if !errors.Is(results[0].Err, context.DeadlineExceeded) || results[1].Err != nil {
		t.Fatalf("results = %#v", results)
	}
}

func TestParseRepositoryRequiresOwnerAndName(t *testing.T) {
	repository, err := doctor.ParseRepository("jozala/omnigrex")
	if err != nil || repository.Owner != "jozala" || repository.Name != "omnigrex" {
		t.Fatalf("ParseRepository() = (%#v, %v)", repository, err)
	}
	for _, value := range []string{"", "omnigrex", "/omnigrex", "jozala/", "jozala/omnigrex/extra", " jozala/omnigrex"} {
		if _, err := doctor.ParseRepository(value); err == nil {
			t.Errorf("ParseRepository(%q) error = nil", value)
		}
	}
}

func TestNewRunnerRejectsInvalidChecks(t *testing.T) {
	for _, checks := range [][]doctor.Check{
		nil,
		{{Name: "", Run: func(context.Context) error { return nil }}},
		{{Name: "configuration"}},
		{{Name: "configuration", Run: func(context.Context) error { return nil }}, {Name: "configuration", Run: func(context.Context) error { return nil }}},
	} {
		if _, err := doctor.NewRunner(time.Second, checks); err == nil {
			t.Errorf("NewRunner(%#v) error = nil", checks)
		}
	}
	if _, err := doctor.NewRunner(0, []doctor.Check{{Name: "configuration", Run: func(context.Context) error { return nil }}}); err == nil {
		t.Error("NewRunner() with zero timeout error = nil")
	}
}

func recordDoctorCheck(order *[]string, name string, err error) func(context.Context) error {
	return func(context.Context) error {
		*order = append(*order, name)
		return err
	}
}
