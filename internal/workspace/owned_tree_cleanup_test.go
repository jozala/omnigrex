package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOwnedTreeCleanupWithLowFileDescriptorLimit(t *testing.T) {
	if os.Getenv("OMNIGREX_TEST_LOW_FD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestOwnedTreeCleanupWithLowFileDescriptorLimit$")
		command.Env = append(os.Environ(), "OMNIGREX_TEST_LOW_FD=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("low-FD cleanup subprocess: %v\n%s", err, output)
		}
		return
	}
	var limits unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limits); err != nil {
		t.Fatal(err)
	}
	original := limits
	limits.Cur = min(limits.Cur, 32)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limits); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original) }()
	root := t.TempDir()
	path := root
	for i := 0; i < 96; i++ {
		path = filepath.Join(path, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := makeOwnedTreeDirectoriesWritable(context.Background(), root); err != nil {
		t.Fatalf("makeOwnedTreeDirectoriesWritable() under low FD limit: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o700 != 0o700 {
		t.Fatalf("deep directory permissions = %v, %v", info, err)
	}
	if err := removeOwnedDirectoryTree(context.Background(), root); err != nil {
		t.Fatalf("removeOwnedDirectoryTree() under low FD limit: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("deep tree still exists: %v", err)
	}
}

func TestOwnedTreeCleanupStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o555); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := makeOwnedTreeDirectoriesWritable(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("makeOwnedTreeDirectoriesWritable() = %v, want cancellation", err)
	}
	if info, err := os.Stat(child); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("canceled traversal changed child permissions: %v, %v", info, err)
	}
}
