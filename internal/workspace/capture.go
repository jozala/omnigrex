package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/uuidtext"
)

type captureManifest struct {
	Version int
	Ref     execution.CaptureRef
}

// Capture seals one immutable operation result and preserves it across restart.
// A retry never overwrites earlier evidence with a later workspace observation.
func (lifecycle *Lifecycle) Capture(ctx context.Context, request execution.CaptureRequest) (execution.CaptureRef, error) {
	destination, err := lifecycle.capturePath(request)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	release, err := lifecycle.acquirePublicationLockContext(ctx, "capture:"+destination)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	defer release()
	ref, found, err := lifecycle.loadSeal(ctx, request)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	if found {
		file, err := lifecycle.OpenCapture(ctx, ref)
		if err != nil {
			return execution.CaptureRef{}, err
		}
		return ref, file.Close()
	}
	if ref, err := lifecycle.findCapture(request); err == nil {
		file, err := lifecycle.openCaptureArtifact(ctx, ref)
		if err != nil {
			return execution.CaptureRef{}, err
		}
		if err := file.Close(); err != nil {
			return execution.CaptureRef{}, err
		}
		return ref, lifecycle.recordSeal(ctx, ref)
	} else if !os.IsNotExist(err) {
		return execution.CaptureRef{}, err
	}
	// A published operation directory with missing metadata is lost evidence,
	// never permission to seal a newer observation under the same identity.
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return execution.CaptureRef{}, ErrInvalidCapture
	}
	if lifecycle.captureSource == nil {
		return execution.CaptureRef{}, execution.ErrUnsupportedRequirement
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return execution.CaptureRef{}, err
	}
	staged, err := os.MkdirTemp(filepath.Dir(destination), ".capture-")
	if err != nil {
		return execution.CaptureRef{}, err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	file, err := os.OpenFile(filepath.Join(staged, "workspace.tar"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	ref = execution.CaptureRef{Request: request}
	err = lifecycle.captureSource.ReadWorkspace(ctx, request, func(reader io.Reader) error {
		var err error
		ref.SHA256, err = normalizeCapture(ctx, reader, file)
		return err
	})
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return execution.CaptureRef{}, err
	}
	manifest, err := json.Marshal(captureManifest{Version: captureVersion, Ref: ref})
	if err != nil {
		return execution.CaptureRef{}, err
	}
	metadata, err := os.OpenFile(filepath.Join(staged, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	_, writeErr := metadata.Write(manifest)
	if err := errors.Join(writeErr, metadata.Sync(), metadata.Close()); err != nil {
		return execution.CaptureRef{}, err
	}
	if err := syncCaptureDirectory(staged); err != nil {
		return execution.CaptureRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return execution.CaptureRef{}, err
	}
	if err := os.Rename(staged, destination); err != nil {
		return execution.CaptureRef{}, err
	}
	if err := syncCaptureDirectory(filepath.Dir(destination)); err != nil {
		return execution.CaptureRef{}, err
	}
	return ref, lifecycle.recordSeal(ctx, ref)
}

func (lifecycle *Lifecycle) loadSeal(ctx context.Context, request execution.CaptureRequest) (execution.CaptureRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return execution.CaptureRef{}, false, err
	}
	if lifecycle.captureLedger != nil {
		return lifecycle.captureLedger.LoadCapture(ctx, request)
	}
	return lifecycle.loadLocalSeal(request)
}

func (lifecycle *Lifecycle) capturePath(request execution.CaptureRequest) (string, error) {
	key := request.Workspace.Key
	if !uuidtext.Valid(key.AssignmentID) || !uuidtext.Valid(key.AgentSessionID) || !uuidtext.Valid(key.AgentTurnID) ||
		key.ExecutionEpoch <= 0 || key.RuntimeProfileName == "" || key.RuntimeProfileVersion == "" || request.OperationID == "" || len(request.OperationID) > 256 || strings.ContainsRune(request.OperationID, '\x00') || request.Purpose != execution.PublicationCapture && request.Purpose != execution.TerminalCapture {
		return "", ErrInvalidCapture
	}
	digest := sha256.Sum256([]byte(request.OperationID))
	return filepath.Join(lifecycle.captureRoot, "assignment-"+key.AssignmentID, key.AgentTurnID+"-"+strconv.FormatInt(key.ExecutionEpoch, 10), string(request.Purpose)+"-"+hex.EncodeToString(digest[:])), nil
}

// Standalone qualification fixtures use a separate local seal inventory.
// Production supplies the Store ledger, independent of filesystem availability.
func (lifecycle *Lifecycle) localSealPath(request execution.CaptureRequest) (string, error) {
	destination, err := lifecycle.capturePath(request)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(destination), ".seals", filepath.Base(destination)+".json"), nil
}

func (lifecycle *Lifecycle) loadLocalSeal(request execution.CaptureRequest) (execution.CaptureRef, bool, error) {
	path, err := lifecycle.localSealPath(request)
	if err != nil {
		return execution.CaptureRef{}, false, err
	}
	content, err := readCaptureMetadata(path)
	if os.IsNotExist(err) {
		return execution.CaptureRef{}, false, nil
	}
	if err != nil {
		return execution.CaptureRef{}, false, err
	}
	var ref execution.CaptureRef
	if json.Unmarshal(content, &ref) != nil {
		return execution.CaptureRef{}, false, ErrInvalidCapture
	}
	digest, digestErr := hex.DecodeString(ref.SHA256)
	if ref.Request != request || digestErr != nil || len(digest) != 32 {
		return execution.CaptureRef{}, false, ErrInvalidCapture
	}
	return ref, true, nil
}

func (lifecycle *Lifecycle) recordSeal(ctx context.Context, ref execution.CaptureRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lifecycle.captureLedger != nil {
		return lifecycle.captureLedger.SealCapture(ctx, ref)
	}
	path, err := lifecycle.localSealPath(ref.Request)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		existing, found, err := lifecycle.loadLocalSeal(ref.Request)
		if err != nil {
			return err
		}
		if !found || existing != ref {
			return execution.ErrConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return syncCaptureDirectory(filepath.Dir(path))
}

func (lifecycle *Lifecycle) findCapture(request execution.CaptureRequest) (execution.CaptureRef, error) {
	destination, err := lifecycle.capturePath(request)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	content, err := readCaptureMetadata(filepath.Join(destination, "manifest.json"))
	if err != nil {
		return execution.CaptureRef{}, err
	}
	var manifest captureManifest
	if json.Unmarshal(content, &manifest) != nil {
		return execution.CaptureRef{}, ErrInvalidCapture
	}
	digest, digestErr := hex.DecodeString(manifest.Ref.SHA256)
	if manifest.Version != captureVersion || manifest.Ref.Request != request || digestErr != nil || len(digest) != 32 {
		return execution.CaptureRef{}, ErrInvalidCapture
	}
	return manifest.Ref, nil
}

func (lifecycle *Lifecycle) OpenCapture(ctx context.Context, ref execution.CaptureRef) (io.ReadCloser, error) {
	sealed, found, err := lifecycle.loadSeal(ctx, ref.Request)
	if err != nil {
		return nil, err
	}
	if !found || sealed != ref {
		return nil, ErrInvalidCapture
	}
	return lifecycle.openCaptureArtifact(ctx, ref)
}

func (lifecycle *Lifecycle) openCaptureArtifact(ctx context.Context, ref execution.CaptureRef) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stored, err := lifecycle.findCapture(ref.Request)
	if err != nil || stored != ref {
		return nil, ErrInvalidCapture
	}
	destination, _ := lifecycle.capturePath(ref.Request)
	file, err := os.Open(filepath.Join(destination, "workspace.tar"))
	if err != nil {
		return nil, err
	}
	if err := VerifyCapture(&captureReader{ctx: ctx, source: file}, ref); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (lifecycle *Lifecycle) withCapture(ctx context.Context, ref execution.CaptureRef, inspect func(string) error) error {
	reader, err := lifecycle.OpenCapture(ctx, ref)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	if err := os.MkdirAll(lifecycle.publicationRoot, 0o700); err != nil {
		return err
	}
	staged, err := os.MkdirTemp(lifecycle.publicationRoot, ".inspect-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := materializeCapture(ctx, reader, staged, ref); err != nil {
		return err
	}
	return inspect(staged)
}

func syncCaptureDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// TerminalCapture finds the retained observation for the original epoch, never
// a replacement checkout. Its operation identity is known before finalization.
func (lifecycle *Lifecycle) TerminalCapture(ctx context.Context, key execution.Key) (execution.CaptureRef, error) {
	request := execution.CaptureRequest{Workspace: execution.WorkspaceRef{Key: key}, OperationID: key.AgentTurnID, Purpose: execution.TerminalCapture}
	ref, found, err := lifecycle.loadSeal(ctx, request)
	if err != nil {
		return execution.CaptureRef{}, err
	}
	if !found {
		return execution.CaptureRef{}, ErrInvalidCapture
	}
	return ref, nil
}

// PublicationObservation supplies the authoritative tip and trusted remote,
// independently of any disposable publication cache.
type PublicationObservation struct {
	RepositoryURL string
	Credential    string
	Revision      string
}

func (PublicationObservation) String() string {
	return "trusted publication observation (<credentials redacted>)"
}
func (PublicationObservation) GoString() string {
	return "workspace.PublicationObservation{<credentials redacted>}"
}

// CaptureTreesEqual privately materializes both sealed evidence and the exact
// trusted revision, even after all original workspace caches have been removed.
func (lifecycle *Lifecycle) CaptureTreesEqual(ctx context.Context, ref execution.CaptureRef, observation PublicationObservation) (bool, error) {
	if err := validateRemote(observation.RepositoryURL); err != nil {
		return false, err
	}
	if err := validateCredential(observation.Credential); err != nil {
		return false, err
	}
	revision, err := normalizeObjectID(observation.Revision)
	if err != nil {
		return false, err
	}
	var equal bool
	err = lifecycle.withCapture(ctx, ref, func(directory string) error {
		trusted, err := os.MkdirTemp(lifecycle.publicationRoot, ".corroborate-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(trusted) }()
		publication := filepath.Join(trusted, "checkout")
		if err := lifecycle.preparePublication(ctx, publication, Publication{RepositoryURL: observation.RepositoryURL, Credential: observation.Credential}, revision); err != nil {
			return err
		}
		equal, err = lifecycle.committedTreesEqual(ctx, directory, publication)
		return err
	})
	return equal, err
}

// VerifyCapture is useful for a transferred artifact before any trusted Git
// operation. Opening a resource is not proof of content integrity.
func VerifyCapture(reader io.Reader, ref execution.CaptureRef) error {
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(reader, maxCaptureBytes+1))
	if err != nil {
		return err
	}
	if n > maxCaptureBytes || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return ErrInvalidCapture
	}
	return nil
}
