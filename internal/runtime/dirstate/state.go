// Package dirstate implements validated Session archives and protected local storage.
package dirstate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// StateSource verifies binding metadata and Store collection authority.
type StateSource interface {
	execution.StateMetadataSource
	CollectionState(context.Context, string) (execution.StateRef, error)
	ListProtectedStates(context.Context) ([]execution.StateRef, error)
}

// RetainedState uses a backend-owned control-plane directory.
// No physical location is exposed to coordination or agent protocol code.
type RetainedState struct {
	root    string
	binding execution.BackendBinding
	source  execution.StateMetadataSource
	cleaner Collector
}

func New(root string, binding execution.BackendBinding, source execution.StateMetadataSource) (*RetainedState, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || binding.Family == "" || binding.Version != 1 || binding.Namespace == "" || source == nil {
		return nil, execution.ErrBindingMismatch
	}
	return &RetainedState{root: root, binding: binding, source: source}, nil
}

type Collector interface {
	EnsureAbsent(context.Context, string, string) error
	Close() error
}

func (state *RetainedState) SetCollector(cleaner Collector) {
	state.cleaner = cleaner
}

func (state *RetainedState) Close() error {
	if state.cleaner != nil {
		return state.cleaner.Close()
	}
	return nil
}

func (state *RetainedState) Binding() execution.BackendBinding { return state.binding }

func (state *RetainedState) location(ctx context.Context, ref execution.StateRef) (string, bool, error) {
	if err := ref.Validate(); err != nil {
		return "", false, err
	}
	if ref.Backend != state.binding || ref.Format != "opencode-data/v1" {
		return "", false, execution.ErrBindingMismatch
	}
	content, initialized, err := state.source.StateMetadata(ctx, ref)
	if err != nil {
		return "", false, err
	}
	var metadata struct {
		Version int    `json:"version"`
		Subpath string `json:"subpath"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil || metadata.Version != 1 || metadata.Subpath != "assignment-"+ref.ParticipantID+"/runtime-state" {
		return "", false, execution.ErrBindingMismatch
	}
	return metadata.Subpath, initialized, nil
}

func (state *RetainedState) Ensure(ctx context.Context, ref execution.StateRef) error {
	location, initialized, err := state.location(ctx, ref)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(state.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if initialized {
		return inspectStateDirectory(root, location)
	}
	parent := path.Dir(location)
	if err := root.Mkdir(parent, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := root.Lstat(parent)
	if err != nil || !info.IsDir() {
		return execution.ErrBindingMismatch
	}
	if err := root.Mkdir(location, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return inspectStateDirectory(root, location)
}

func inspectStateDirectory(root *os.Root, location string) error {
	// os.Root prevents escape; Lstat additionally rejects internal symlink aliases.
	for _, name := range []string{path.Dir(location), location} {
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return execution.ErrStateUnavailable
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return execution.ErrBindingMismatch
		}
	}
	return nil
}

func (state *RetainedState) Inspect(ctx context.Context, ref execution.StateRef) error {
	location, initialized, err := state.location(ctx, ref)
	if err != nil {
		return err
	}
	if !initialized {
		return nil
	} // Not yet prepared is distinct from lost protected data.
	root, err := os.OpenRoot(state.root)
	if err != nil {
		return err
	}
	defer root.Close()
	return inspectStateDirectory(root, location)
}

func (state *RetainedState) Check(ctx context.Context) error {
	source, ok := state.source.(StateSource)
	if !ok {
		return execution.ErrBindingMismatch
	}
	refs, err := source.ListProtectedStates(ctx)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := state.Inspect(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

func (state *RetainedState) Collect(ctx context.Context, ref execution.StateRef) error {
	if ref.SessionID != "" {
		return execution.ErrBindingMismatch
	}
	source, ok := state.source.(StateSource)
	if !ok {
		return execution.ErrBindingMismatch
	}
	allowed, err := source.CollectionState(ctx, ref.ParticipantID)
	if err != nil {
		return err
	}
	if allowed != ref {
		return execution.ErrBindingMismatch
	}
	location, _, err := state.location(ctx, ref)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(state.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := inspectStateDirectory(root, location); err != nil && !errors.Is(err, execution.ErrStateUnavailable) {
		return err
	}
	parent, err := root.OpenRoot(path.Dir(location))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	entries, err := fs.ReadDir(parent.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if restoreStagingName(entry.Name()) {
			if err := removeStateTree(parent, entry.Name()); err != nil {
				return fmt.Errorf("collect restore staging: %w", err)
			}
		}
	}
	if state.cleaner != nil {
		return state.cleaner.EnsureAbsent(ctx, ref.ParticipantID, location)
	}
	return removeStateTree(parent, path.Base(location))
}

func restoreStagingName(name string) bool {
	const prefix = ".restore-"
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+32 {
		return false
	}
	for _, character := range name[len(prefix):] {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

// State archives are bounded, checksummed tar files with a final manifest.
// They contain regular Session files and directories only, never links/devices,
// credential files, caches, workspaces, or execution snapshots.
const stateArchiveLimit = 256 << 20
const stateArchiveEncodedLimit = 384 << 20
const stateArchiveManifestLimit = 16 << 20
const stateArchiveEntries = 100_000
const stateManifestName = ".omnigrex-state-manifest.json"

type stateArchiveEntry struct {
	Name      string `json:"name"`
	Mode      int64  `json:"mode"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	Directory bool   `json:"directory,omitempty"`
}
type stateArchiveManifest struct {
	Version int                 `json:"version"`
	State   execution.StateRef  `json:"state"`
	Entries []stateArchiveEntry `json:"entries"`
}

func safeStateName(name string) bool {
	if name == "." || name == ".." || name == "" || len(name) > 4096 || name == stateManifestName || path.Clean(name) != name || strings.HasPrefix(name, "../") || path.IsAbs(name) || strings.ContainsAny(name, "\\\x00") {
		return false
	}
	// OpenCode receives credentials in its ephemeral environment; fail closed
	// if a credential file was nevertheless written into Session storage.
	base := strings.ToLower(path.Base(name))
	return base != "auth.json" && base != "provider-credentials.json" && base != ".env"
}

func (state *RetainedState) Export(ctx context.Context, ref execution.StateRef, output io.Writer) error {
	if ref.SessionID != "" {
		return execution.ErrBindingMismatch
	}
	location, _, err := state.location(ctx, ref)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(state.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := inspectStateDirectory(root, location); err != nil {
		return err
	}
	data, err := root.OpenRoot(location)
	if err != nil {
		return err
	}
	defer data.Close()
	return exportStateDirectory(ctx, ref, data, output)
}

func exportStateDirectory(ctx context.Context, ref execution.StateRef, data *os.Root, output io.Writer) error {
	manifest := stateArchiveManifest{Version: 1, State: ref, Entries: []stateArchiveEntry{}}
	writer := tar.NewWriter(&stateLimitedWriter{Writer: output, remaining: stateArchiveEncodedLimit})
	var total int64
	err := fs.WalkDir(data.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if !safeStateName(name) || len(manifest.Entries) >= stateArchiveEntries {
			return execution.ErrCorruptState
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return execution.ErrCorruptState
		}
		record := stateArchiveEntry{Name: name, Mode: int64(info.Mode().Perm()), Directory: info.IsDir()}
		header := &tar.Header{Name: name, Mode: record.Mode, Typeflag: tar.TypeDir}
		if !record.Directory {
			record.Size = info.Size()
			total += record.Size
			if total > stateArchiveLimit || record.Size < 0 {
				return execution.ErrCorruptState
			}
			header.Size, header.Typeflag = record.Size, tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if !record.Directory {
			file, err := data.Open(name)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(writer, hash), io.LimitReader(file, record.Size+1))
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
			if n != record.Size {
				return execution.ErrCorruptState
			}
			record.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
		manifest.Entries = append(manifest.Entries, record)
		return nil
	})
	if err != nil {
		return err
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(content) > stateArchiveManifestLimit {
		return execution.ErrCorruptState
	}
	if err := writer.WriteHeader(&tar.Header{Name: stateManifestName, Mode: 0600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := writer.Write(content); err != nil {
		return err
	}
	return writer.Close()
}

// Restore validates a complete archive in a sibling staging directory before
// atomic publication. Existing data is never overwritten or merged.
func (state *RetainedState) Restore(ctx context.Context, ref execution.StateRef, input io.Reader) (err error) {
	if ref.SessionID != "" {
		return execution.ErrBindingMismatch
	}
	location, _, err := state.location(ctx, ref)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(state.root)
	if err != nil {
		return err
	}
	defer root.Close()
	parent := path.Dir(location)
	if err := root.Mkdir(parent, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := root.Lstat(parent)
	if err != nil || !info.IsDir() {
		return execution.ErrBindingMismatch
	}
	// All creation and extraction stays behind os.Root, including staging.
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	stageName := path.Join(parent, ".restore-"+hex.EncodeToString(random[:]))
	if err := root.Mkdir(stageName, 0700); err != nil {
		return err
	}
	defer func() {
		if cleanupErr := removeStateTree(root, stageName); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove restore staging: %w", cleanupErr))
		}
	}()
	data, err := root.OpenRoot(stageName)
	if err != nil {
		return err
	}
	defer data.Close()
	if err := restoreStateArchive(ctx, ref, input, data); err != nil {
		return err
	}
	if _, err := root.Lstat(location); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		if err := inspectStateDirectory(root, location); err != nil {
			return err
		}
		existing, err := root.OpenRoot(location)
		if err != nil {
			return err
		}
		defer existing.Close()
		actual, desired := sha256.New(), sha256.New()
		if err := exportStateDirectory(ctx, ref, existing, actual); err != nil {
			return err
		}
		if err := exportStateDirectory(ctx, ref, data, desired); err != nil {
			return err
		}
		if !bytes.Equal(actual.Sum(nil), desired.Sum(nil)) {
			return execution.ErrConflict
		}
		return nil // Retry after a lost successful restore acknowledgement.
	}
	if err := root.Rename(stageName, location); err != nil {
		return err
	}
	directory, err := root.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// removeStateTree repairs only disposable directories before removal. Archive
// modes can deny traversal or deletion even to their non-root owner. WalkDir
// never follows links, and all access remains confined to the supplied root.
func removeStateTree(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := root.Chmod(name, 0700); err != nil {
			return err
		}
		directory, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		walkErr := fs.WalkDir(directory.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return directory.Chmod(name, 0700)
			}
			return nil
		})
		if err := errors.Join(walkErr, directory.Close()); err != nil {
			return err
		}
	}
	return root.RemoveAll(name)
}

type stateCountingReader struct {
	io.Reader
	count int64
}

type stateLimitedWriter struct {
	io.Writer
	remaining int64
}

func (writer *stateLimitedWriter) Write(buffer []byte) (int, error) {
	if int64(len(buffer)) > writer.remaining {
		return 0, execution.ErrCorruptState
	}
	n, err := writer.Writer.Write(buffer)
	writer.remaining -= int64(n)
	return n, err
}

func (reader *stateCountingReader) Read(buffer []byte) (int, error) {
	n, err := reader.Reader.Read(buffer)
	reader.count += int64(n)
	return n, err
}

func restoreStateArchive(ctx context.Context, ref execution.StateRef, input io.Reader, data *os.Root) error {
	limited := &io.LimitedReader{R: input, N: stateArchiveEncodedLimit + 1}
	counted := &stateCountingReader{Reader: limited}
	reader := tar.NewReader(counted)
	entries := []stateArchiveEntry{}
	seen := make(map[string]bool)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf("%w: missing complete manifest", execution.ErrCorruptState)
		}
		if header.Name == stateManifestName {
			if header.Typeflag != tar.TypeReg || header.Size > stateArchiveManifestLimit {
				return execution.ErrCorruptState
			}
			var manifest stateArchiveManifest
			decoder := json.NewDecoder(reader)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&manifest); err != nil {
				return execution.ErrCorruptState
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return execution.ErrCorruptState
			}
			if manifest.State != ref {
				return execution.ErrBindingMismatch
			}
			want, _ := json.Marshal(entries)
			actual, _ := json.Marshal(manifest.Entries)
			if manifest.Version != 1 || !bytes.Equal(want, actual) {
				return execution.ErrCorruptState
			}
			before := counted.count
			if _, err := reader.Next(); err != io.EOF || counted.count-before < 1024 {
				return execution.ErrCorruptState
			}
			// Reject hidden entries/data after tar EOF, and require a bounded EOF.
			trailing, err := io.ReadAll(limited)
			if err != nil || limited.N == 0 || len(bytes.Trim(trailing, "\x00")) != 0 {
				return execution.ErrCorruptState
			}
			// Preserve restrictive directory modes after children are materialized.
			for index := len(entries) - 1; index >= 0; index-- {
				if entries[index].Directory {
					directory, err := data.Open(entries[index].Name)
					if err != nil {
						return err
					}
					if err := errors.Join(directory.Chmod(fs.FileMode(entries[index].Mode)), directory.Sync(), directory.Close()); err != nil {
						return err
					}
				}
			}
			root, err := data.Open(".")
			if err != nil {
				return err
			}
			return errors.Join(root.Sync(), root.Close())
		}
		if !safeStateName(header.Name) || seen[header.Name] || len(entries) >= stateArchiveEntries || header.Mode < 0 || header.Mode > 0777 || header.Size < 0 {
			return execution.ErrCorruptState
		}
		seen[header.Name] = true
		record := stateArchiveEntry{Name: header.Name, Mode: header.Mode, Size: header.Size, Directory: header.Typeflag == tar.TypeDir}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return execution.ErrCorruptState
			}
			if err := data.Mkdir(header.Name, 0700); err != nil {
				return execution.ErrCorruptState
			}
		case tar.TypeReg:
			total += header.Size
			if total > stateArchiveLimit {
				return execution.ErrCorruptState
			}
			file, err := data.OpenFile(header.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(header.Mode))
			if err != nil {
				return execution.ErrCorruptState
			}
			hash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
			modeErr := file.Chmod(fs.FileMode(header.Mode))
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil || modeErr != nil || syncErr != nil || closeErr != nil {
				return errors.Join(copyErr, modeErr, syncErr, closeErr)
			}
			if n != header.Size {
				return execution.ErrCorruptState
			}
			record.SHA256 = hex.EncodeToString(hash.Sum(nil))
		default:
			return execution.ErrCorruptState
		}
		entries = append(entries, record)
	}
}
