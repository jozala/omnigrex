package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStorageFenceExcludesCollectionAndRejectsLateWriters(t *testing.T) {
	l, _ := cleanupFixture(t)
	var collecting atomic.Bool
	l.storageWriter = func(ctx context.Context, _ string, operation func(context.Context) error) error {
		if collecting.Load() {
			return ErrCleanupIncomplete
		}
		return operation(ctx)
	}
	other, err := New(Options{WorkspaceRoot: l.workspaceRoot, PublicationRoot: l.publicationRoot, MiseRoot: l.miseRoot, StorageWriter: l.storageWriter})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- l.WithParticipantStorageWriter(context.Background(), cleanupID, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	collecting.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := other.WithParticipantStorageCollection(ctx, cleanupID, func(context.Context) error { t.Error("collected while stale preparation still writes"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusion=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := other.WithParticipantStorageCollection(context.Background(), cleanupID, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := l.WithParticipantStorageWriter(context.Background(), cleanupID, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatalf("old-Participant writer admitted: %v %v", called, err)
	}
}

func TestStorageFenceInheritedDescriptorKeepsChildExcludedAfterOwnerCloses(t *testing.T) {
	l, root := cleanupFixture(t)
	l.storageWriter = func(ctx context.Context, _ string, operation func(context.Context) error) error {
		return operation(ctx)
	}
	var child *exec.Cmd
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	err = l.WithParticipantStorageWriter(context.Background(), cleanupID, func(ctx context.Context) error {
		// Verify the production Git launcher actually passes the live physical lock.
		wrapper := filepath.Join(root, "git-lock-fixture")
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntest -f /proc/self/fd/3 || exit 75\nprintf inherited\n"), 0700); err != nil {
			return err
		}
		l.gitExecutable = wrapper
		output, err := l.gitOutput(ctx, "lock fixture", "", "", nil)
		if err != nil || strings.TrimSpace(output) != "inherited" {
			t.Fatalf("Git lock inheritance=%q %v", output, err)
		}
		scope := ctx.Value(storageFenceKey{}).(*storageFenceScope)
		child = exec.Command("sh", "-c", "read value")
		child.Stdin = reader
		child.ExtraFiles = []*os.File{scope.file}
		return child.Start()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.WithParticipantStorageCollection(ctx, cleanupID, func(context.Context) error { t.Error("child lock released with parent descriptor"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := l.WithParticipantStorageCollection(context.Background(), cleanupID, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
