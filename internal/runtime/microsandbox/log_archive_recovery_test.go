package microsandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"golang.org/x/sys/unix"
)

type replacingArchivePlatform struct {
	*fakePlatform
	lookups   int
	replaceAt int
	replace   func()
}

func (driver *replacingArchivePlatform) Lookup(ctx context.Context, name string) (metadata, error) {
	observed, err := driver.fakePlatform.Lookup(ctx, name)
	driver.lookups++
	if driver.lookups == driver.replaceAt {
		driver.replace()
	}
	return observed, err
}

func TestRuntimeLogsArchiveRejectsReplacementDuringSourceBinding(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(map[int]string{1: "before open", 2: "after open"}[at], func(t *testing.T) {
			backend, meta, source, destination := archiveFixture(t)
			writeArchiveFile(t, filepath.Join(source, "exec.log"), "original")
			driver := backend.platform.(*fakePlatform)
			backend.platform = &replacingArchivePlatform{fakePlatform: driver, replaceAt: at, replace: func() {
				if err := os.Rename(source, source+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				writeArchiveFile(t, filepath.Join(source, "exec.log"), "replacement private data")
				replacement := meta
				replacement.ID = "replacement"
				driver.resources[meta.Name] = replacement
			}}
			if err := backend.archiveRuntimeLogs(context.Background(), testRequest().Key, meta); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("replacement was not rejected: %v", err)
			}
			if content, err := os.ReadFile(filepath.Join(destination, "exec.log")); err == nil {
				t.Fatalf("published stale archive: %q", content)
			}
		})
	}
}

func TestRuntimeLogsArchiveRecoversCrashStages(t *testing.T) {
	if directory := os.Getenv("OMNIGREX_ARCHIVE_CRASH_FIXTURE"); directory != "" {
		file, err := os.Open(directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		_ = publishLogFile(context.Background(), file, "exec.log", func(writer io.Writer) error {
			_, _ = writer.Write([]byte(strings.Repeat("partial native output", 4096)))
			os.Exit(17)
			return nil
		})
		t.Fatal("fixture did not crash")
	}
	backend, meta, source, destination := archiveFixture(t)
	writeArchiveFile(t, filepath.Join(source, "exec.log"), "complete")
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		command := exec.Command(binary, "-test.run=^TestRuntimeLogsArchiveRecoversCrashStages$")
		command.Env = append(os.Environ(), "OMNIGREX_ARCHIVE_CRASH_FIXTURE="+destination)
		err := command.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 17 {
			t.Fatalf("fixture exit: %v", err)
		}
	}
	if err := backend.archiveRuntimeLogs(context.Background(), testRequest().Key, meta); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".archive-") {
			t.Fatal("abandoned stage survived recovery", entry.Name())
		}
	}
	if content, _ := os.ReadFile(filepath.Join(destination, "exec.log")); string(content) != "complete" {
		t.Fatal("completed log not preserved")
	}
}

func TestRuntimeLogsArchiveDoesNotReclaimConcurrentStage(t *testing.T) {
	backend, meta, source, destination := archiveFixture(t)
	writeArchiveFile(t, filepath.Join(source, "exec.log"), "source")
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(directory.Fd()), unix.LOCK_UN)
	// Represents a still-active writer holding the resource-scoped lock.
	stage := ".archive-AAAAAAAAAAAAAAAAAAAAAAAAAA"
	writeArchiveFile(t, filepath.Join(destination, stage), "in progress")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := backend.archiveRuntimeLogs(ctx, testRequest().Key, meta); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent archive bypassed serialization: %v", err)
	}
	if content, _ := os.ReadFile(filepath.Join(destination, stage)); string(content) != "in progress" {
		t.Fatal("active stage was reclaimed")
	}
}
