package dirstate

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// ValidateArchive checks a supplemental archive without granting attachment or
// restore authority. The same codec verifies inventory, checksums and complete
// termination in private disposable storage; no retained resource is touched.
func ValidateArchive(ctx context.Context, input io.ReadSeeker) (err error) {
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := tar.NewReader(io.LimitReader(input, stateArchiveEncodedLimit+1))
	var ref execution.StateRef
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err != nil {
			return execution.ErrCorruptState
		}
		if header.Name != stateManifestName {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size > stateArchiveManifestLimit {
			return execution.ErrCorruptState
		}
		var manifest stateArchiveManifest
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.State.Validate() != nil {
			return execution.ErrCorruptState
		}
		ref = manifest.State
		break
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "omnigrex-validate-state-")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	defer func() {
		entries, readErr := fs.ReadDir(root.FS(), ".")
		err = errors.Join(err, readErr)
		for _, entry := range entries {
			err = errors.Join(err, removeStateTree(root, entry.Name()))
		}
		err = errors.Join(err, root.Close(), os.Remove(directory))
	}()
	return restoreStateArchive(ctx, ref, input, root)
}
