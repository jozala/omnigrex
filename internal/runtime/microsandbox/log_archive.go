package microsandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"golang.org/x/sys/unix"
)

const runtimeLogArchiveWait = 5 * time.Second

type runtimeLogArchive struct {
	Key      execution.Key     `json:"key"`
	Resource metadata          `json:"resource"`
	Captured time.Time         `json:"captured"`
	Files    map[string]string `json:"files"`
}

// Called only after existing cleanup authorization and confirmed stop. Diagnostic
// copies cannot establish flush/stop receipts and must not impede native cleanup.
func (backend *Backend) removeResource(ctx context.Context, resource metadata) error {
	var key execution.Key
	if malformedResource(resource) == "" && resource.Labels[diagnosticLabel] == "" && json.Unmarshal([]byte(resource.Labels[identityLabel]), &key) == nil {
		copyCtx, cancel := context.WithTimeout(ctx, runtimeLogArchiveWait)
		err := backend.archiveRuntimeLogs(copyCtx, key, resource)
		cancel()
		if err != nil {
			attrs := append(resourceLogAttrs(resource), slog.String("error", err.Error()))
			slog.Default().LogAttrs(ctx, slog.LevelWarn, "Microsandbox native log archive incomplete", attrs...)
		}
	}
	return backend.platform.Remove(ctx, resource)
}

func (backend *Backend) archiveRuntimeLogs(ctx context.Context, key execution.Key, resource metadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var owner execution.Key
	if !validKey(key) || malformedResource(resource) != "" || resource.Labels[namespaceLabel] != backend.options.Namespace || resource.Labels[diagnosticLabel] != "" || json.Unmarshal([]byte(resource.Labels[identityLabel]), &owner) != nil || owner != key || resource.ID == "" || resource.Name == "." || resource.Name == ".." || strings.ContainsAny(resource.Name, "/\\\x00") {
		return execution.ErrConflict
	}
	actual, err := backend.resolveRef(ctx, resourceRef(resource))
	if err != nil {
		return err
	}
	if !maps.Equal(actual.Labels, resource.Labels) || !terminalEnvironmentStatus(actual.Status) {
		return execution.ErrConflict
	}
	digest := sha256.Sum256([]byte(resource.ID))
	destination, err := openLogDirectory(filepath.Join(backend.options.StateRoot, "assignment-"+key.AssignmentID, "runtime-logs", key.AgentTurnID+"-"+strconv.FormatInt(key.ExecutionEpoch, 10), hex.EncodeToString(digest[:])), true)
	if err != nil {
		return err
	}
	defer destination.Close()
	manifest := runtimeLogArchive{Key: key, Resource: resource, Captured: time.Now().UTC(), Files: map[string]string{"runtime.log": "missing", "kernel.log": "missing", "exec.log": "missing"}}
	source, err := openLogDirectory(filepath.Join(backend.options.Home, "sandboxes", resource.Name, "logs"), false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var failures []error
	if source != nil {
		defer source.Close()
		entries, err := source.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !nativeLogFilename(name) {
				continue
			}
			manifest.Files[name] = "failed"
			if err := copyNativeLog(ctx, source, destination, name); err != nil {
				failures = append(failures, fmt.Errorf("copy %s: %w", name, err))
			} else {
				manifest.Files[name] = "copied"
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	content, err := json.Marshal(manifest)
	if err == nil {
		err = publishLogFile(ctx, destination, "archive.json", func(writer io.Writer) error { _, err := writer.Write(content); return err })
	}
	return errors.Join(append(failures, err)...)
}

func nativeLogFilename(name string) bool {
	for _, base := range []string{"runtime.log", "kernel.log", "exec.log"} {
		if name == base {
			return true
		}
		if suffix, ok := strings.CutPrefix(name, base+"."); ok {
			number, err := strconv.ParseUint(suffix, 10, 64)
			return err == nil && number > 0 && strconv.FormatUint(number, 10) == suffix
		}
	}
	return false
}

// Open every component relative to its parent without following symlinks,
// including trusted-root ancestors. No guest-controlled path is accepted.
func openLogDirectory(location string, create bool) (*os.File, error) {
	if !filepath.IsAbs(location) || filepath.Clean(location) != location {
		return nil, execution.ErrInvalidSpec
	}
	parent, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(location, "/"), "/") {
		fd, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(int(parent.Fd()), part, 0700); err == nil || errors.Is(err, unix.EEXIST) {
				fd, err = unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		_ = parent.Close()
		if err != nil {
			return nil, err
		}
		parent = os.NewFile(uintptr(fd), part)
	}
	return parent, nil
}

func copyNativeLog(ctx context.Context, source, destination *os.File, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Openat(int(source.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return errors.New("native log must be a regular file with one link")
	}
	return publishLogFile(ctx, destination, name, func(writer io.Writer) error {
		buffer := make([]byte, 64<<10)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := file.Read(buffer)
			if n > 0 {
				if _, err := writer.Write(buffer[:n]); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	})
}

func publishLogFile(ctx context.Context, directory *os.File, name string, write func(io.Writer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var existing unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), name, &existing, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil && (existing.Mode&unix.S_IFMT != unix.S_IFREG || existing.Nlink != 1) {
		return errors.New("unsafe existing archive entry")
	}
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	staging := ".archive-" + rand.Text()
	fd, err := unix.Openat(int(directory.Fd()), staging, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), staging)
	defer func() { _ = file.Close(); _ = unix.Unlinkat(int(directory.Fd()), staging, 0) }()
	if err := write(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), staging, int(directory.Fd()), name); err != nil {
		return err
	}
	return directory.Sync()
}
