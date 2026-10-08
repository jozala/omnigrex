package workspace

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

const captureVersion = 1
const maxCaptureBytes int64 = 1 << 30
const maxCaptureEntries = 100000

var ErrInvalidCapture = errors.New("invalid or incomplete Workspace Capture")

// normalizeCapture fixes a portable encoding independently of backend tar
// metadata. All inventory is retained, including ignored entries. No entry is
// silently omitted when an encoding, type or resource limit is unsupported.
func normalizeCapture(ctx context.Context, source io.Reader, destination io.Writer) (string, error) {
	input := &captureReader{ctx: ctx, source: io.LimitReader(source, maxCaptureBytes+1)}
	reader := tar.NewReader(input)
	hash := sha256.New()
	writer := tar.NewWriter(&captureWriter{destination: io.MultiWriter(destination, hash)})
	inventory := captureEntryInventory{entries: make(map[string]byte)}
	var root string
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		before := input.bytes
		header, err := reader.Next()
		if err == io.EOF {
			if input.bytes-before < 1024 || captureTail(input) != nil {
				return "", ErrInvalidCapture
			}
			break
		}
		if err != nil {
			return "", ErrInvalidCapture
		}
		if _, opaque := header.PAXRecords["SCHILY.xattr.user.msb.override_stat"]; opaque {
			return "", ErrInvalidCapture
		}
		name := strings.TrimSuffix(header.Name, "/")
		if root == "" {
			root = strings.Split(name, "/")[0]
			if root == "" || root == "." || root == ".." || header.Typeflag != tar.TypeDir || name != root {
				return "", ErrInvalidCapture
			}
			if header.Size != 0 {
				return "", ErrInvalidCapture
			}
			continue
		}
		if name == root {
			return "", ErrInvalidCapture
		}
		name, found := strings.CutPrefix(name, root+"/")
		if !found {
			return "", ErrInvalidCapture
		}
		header.Name = name
		if err := inventory.add(header); err != nil {
			return "", err
		}
		normal := &tar.Header{Name: name, Typeflag: header.Typeflag, Size: header.Size, Linkname: header.Linkname,
			Mode: header.Mode & 0o777, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := writer.WriteHeader(normal); err != nil {
			return "", err
		}
		if _, err := io.CopyN(writer, reader, header.Size); err != nil {
			return "", ErrInvalidCapture
		}
	}
	if !inventory.complete() {
		return "", ErrInvalidCapture
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func safeCapturePath(name string) bool {
	return name != "" && name != "." && path.Clean(name) == name && !path.IsAbs(name) &&
		!strings.ContainsAny(name, "\\\x00") && name != ".." && !strings.HasPrefix(name, "../") && len(name) <= 4096
}

// materializeCapture never follows captured symlinks or runs captured hooks or
// configuration. Git is invoked only after metadata has been neutralized.
func materializeCapture(ctx context.Context, source io.Reader, destination string, ref execution.CaptureRef) error {
	verified, cleanup, err := verifiedCaptureInput(ctx, source, filepath.Dir(destination), ref)
	if err != nil {
		return err
	}
	defer cleanup()
	hash := sha256.New()
	input := &captureReader{ctx: ctx, source: io.TeeReader(io.LimitReader(verified, maxCaptureBytes+1), hash)}
	reader := tar.NewReader(input)
	inventory := captureEntryInventory{entries: make(map[string]byte)}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := input.bytes
		header, err := reader.Next()
		if err == io.EOF {
			if input.bytes-before < 1024 || captureTail(input) != nil {
				return ErrInvalidCapture
			}
			break
		}
		if err != nil {
			return ErrInvalidCapture
		}
		if err := inventory.add(header); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.Mkdir(target, 0o700)
		case tar.TypeReg, tar.TypeRegA:
			var file *os.File
			file, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode&0o777)|0o600)
			if err == nil {
				_, err = io.CopyN(file, reader, header.Size)
				err = errors.Join(err, file.Close())
			}
		case tar.TypeSymlink:
			err = os.Symlink(header.Linkname, target)
		default:
			return ErrInvalidCapture
		}
		if err != nil {
			return fmt.Errorf("materialize Workspace Capture: %w", ErrInvalidCapture)
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return ErrInvalidCapture
	}
	if !inventory.complete() {
		return ErrInvalidCapture
	}
	for _, unsafe := range []string{"info/grafts", "shallow", "objects/info/alternates", "objects/info/http-alternates", "commondir", "refs/replace"} {
		if _, err := os.Lstat(filepath.Join(destination, ".git", unsafe)); !os.IsNotExist(err) {
			return ErrInvalidPublicationHistory
		}
	}
	// Packed replacement refs must be rejected before trusted Git sees the repo.
	if refs, err := os.ReadFile(filepath.Join(destination, ".git/packed-refs")); err == nil && strings.Contains(string(refs), " refs/replace/") {
		return ErrInvalidPublicationHistory
	}
	for _, unsafe := range []string{"config", "config.worktree", "hooks", "info/attributes"} {
		if err := os.RemoveAll(filepath.Join(destination, ".git", unsafe)); err != nil {
			return ErrInvalidCapture
		}
	}
	return writeTrustedCaptureConfig(destination)
}

type captureEntryInventory struct {
	entries map[string]byte
	bytes   int64
}

func (inventory *captureEntryInventory) add(header *tar.Header) error {
	name := header.Name
	if !safeCapturePath(name) || len(inventory.entries) >= maxCaptureEntries {
		return ErrInvalidCapture
	}
	if _, exists := inventory.entries[name]; exists {
		return ErrInvalidCapture
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if inventory.entries[parent] != tar.TypeDir {
			return ErrInvalidCapture
		}
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeSymlink:
	default:
		return ErrInvalidCapture
	}
	if header.Size < 0 || header.Size > maxCaptureBytes-inventory.bytes || len(header.Linkname) > 4096 {
		return ErrInvalidCapture
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Size != 0 {
		return ErrInvalidCapture
	}
	if strings.HasPrefix(name, ".git/") && header.Typeflag == tar.TypeSymlink {
		return ErrInvalidCapture
	}
	if _, opaque := header.PAXRecords["SCHILY.xattr.user.msb.override_stat"]; opaque {
		return ErrInvalidCapture
	}
	inventory.entries[name] = header.Typeflag
	inventory.bytes += header.Size
	return nil
}

func (inventory *captureEntryInventory) complete() bool {
	return inventory.entries[".git"] == tar.TypeDir && inventory.entries[".git/HEAD"] == tar.TypeReg && inventory.entries[".git/index"] == tar.TypeReg && inventory.entries[".git/objects"] == tar.TypeDir
}

// Derive only the repository object format from the captured HEAD identity.
// No captured Git setting, include or alias becomes trusted configuration.
func writeTrustedCaptureConfig(directory string) error {
	head, err := readCaptureMetadata(filepath.Join(directory, ".git/HEAD"))
	if err != nil {
		return ErrInvalidCapture
	}
	revision := strings.TrimSpace(string(head))
	if ref, symbolic := strings.CutPrefix(revision, "ref: "); symbolic {
		if !safeCapturePath(ref) || !strings.HasPrefix(ref, "refs/") {
			return ErrInvalidCapture
		}
		value, err := readCaptureMetadata(filepath.Join(directory, ".git", filepath.FromSlash(ref)))
		if err == nil {
			revision = strings.TrimSpace(string(value))
		} else if os.IsNotExist(err) {
			packed, err := readCaptureMetadata(filepath.Join(directory, ".git/packed-refs"))
			if err != nil {
				return ErrInvalidCapture
			}
			revision = ""
			for _, line := range strings.Split(string(packed), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[1] == ref {
					revision = fields[0]
					break
				}
			}
		} else {
			return ErrInvalidCapture
		}
	}
	object, err := normalizeObjectID(revision)
	if err != nil {
		return ErrInvalidCapture
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = false\n\tfilemode = true\n"
	if len(object) == 64 {
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = false\n\tfilemode = true\n[extensions]\n\tobjectformat = sha256\n"
	}
	return os.WriteFile(filepath.Join(directory, ".git/config"), []byte(config), 0o600)
}

func readCaptureMetadata(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	const maxMetadataBytes = 8 << 20
	content, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil || len(content) > maxMetadataBytes {
		return nil, ErrInvalidCapture
	}
	return content, nil
}

func verifiedCaptureInput(ctx context.Context, source io.Reader, directory string, ref execution.CaptureRef) (io.Reader, func(), error) {
	cleanup := func() {}
	seeker, seekable := source.(io.ReadSeeker)
	if !seekable {
		file, err := os.CreateTemp(directory, ".capture-input-")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { _ = file.Close(); _ = os.Remove(file.Name()) }
		_, err = io.Copy(file, &captureReader{ctx: ctx, source: io.LimitReader(source, maxCaptureBytes+1)})
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		seeker = file
	}
	if err := VerifyCapture(&captureReader{ctx: ctx, source: seeker}, ref); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return seeker, cleanup, nil
}

type captureReader struct {
	ctx    context.Context
	source io.Reader
	bytes  int64
}

func (reader *captureReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.source.Read(buffer)
	reader.bytes += int64(n)
	if reader.bytes > maxCaptureBytes {
		return n, ErrInvalidCapture
	}
	return n, err
}

// Only zero block padding may follow the archive terminator. Concatenated
// archives and nonzero tails cannot silently disappear from the inventory.
func captureTail(reader io.Reader) error {
	buffer := make([]byte, 32768)
	for {
		n, err := reader.Read(buffer)
		for _, value := range buffer[:n] {
			if value != 0 {
				return ErrInvalidCapture
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type captureWriter struct {
	destination io.Writer
	bytes       int64
}

func (writer *captureWriter) Write(buffer []byte) (int, error) {
	if int64(len(buffer)) > maxCaptureBytes-writer.bytes {
		return 0, ErrInvalidCapture
	}
	n, err := writer.destination.Write(buffer)
	writer.bytes += int64(n)
	return n, err
}
