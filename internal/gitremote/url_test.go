package gitremote_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/gitremote"
)

func TestBaseURLDefaultsAndConstructsRepositoryURLs(t *testing.T) {
	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{name: "default", want: "https://github.com/acme/widgets.git"},
		{name: "enterprise path", base: "https://github.enterprise.test/source/", want: "https://github.enterprise.test/source/acme/widgets.git"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, err := gitremote.ParseBaseURL(test.base)
			if err != nil {
				t.Fatalf("ParseBaseURL() error = %v", err)
			}
			got, err := base.RepositoryURL("acme", "widgets")
			if err != nil || got != test.want {
				t.Fatalf("RepositoryURL() = (%q, %v), want %q", got, err, test.want)
			}
		})
	}
}

func TestBaseURLRejectsUnsafeValuesWithoutDisclosure(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "insecure", value: "http://github.enterprise.test/source"},
		{name: "credentialed", value: "https://credential-sentinel@github.enterprise.test/source"},
		{name: "unclean path", value: "https://github.enterprise.test/source/../repos"},
		{name: "escaped path", value: "https://github.enterprise.test/source%2Frepos"},
		{name: "query", value: "https://github.enterprise.test/source?credential-sentinel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := gitremote.ParseBaseURL(test.value)
			if !errors.Is(err, gitremote.ErrInvalidBaseURL) {
				t.Fatalf("ParseBaseURL() error = %v", err)
			}
			if strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("ParseBaseURL() disclosed credentials: %v", err)
			}
		})
	}
}
