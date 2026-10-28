// Package gitdelivery validates additive, credential-free Git snapshot delivery.
// It never runs Git against an agent-controlled repository.
package gitdelivery

import (
	"archive/tar"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"io"
	"path"
	"regexp"
	"strings"
)

const MaxPackBytes = 256 << 20
const MaxArchiveBytes = 1 << 30

// Loose-ref delivery supports only the controller-prepared files backend.
// Includes can hide an extension declaration, so reject them rather than parse
// or follow agent-controlled configuration paths.
var unsupportedGitSection = regexp.MustCompile(`(?im)^\s*\[\s*(extensions|include|includeif)(\s|\]|")`)

var ErrUnsafe = errors.New("unsafe or unsupported workspace Git metadata")

// Snapshot contains a complete, independently verified SHA-1 pack and index.
// Ref is a controller-derived immutable operation reference, never agent input.
type Snapshot = execution.GitSnapshot
type File struct {
	Name      string
	Content   []byte
	Directory bool
	UID, GID  int
}

func Files(s Snapshot) ([]File, error) {
	if !hexString(s.SHA, 40) || !strings.HasPrefix(s.Ref, "refs/omnigrex/default/") || !hexString(strings.TrimPrefix(s.Ref, "refs/omnigrex/default/"), 64) || !hexString(s.PackName, 40) || len(s.Pack) == 0 || len(s.Pack) > MaxPackBytes || len(s.Index) == 0 || len(s.Index) > MaxPackBytes {
		return nil, ErrUnsafe
	}
	if len(s.Pack) < 32 || len(s.Index) < 1072 || !bytes.Equal(s.Pack[:8], []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2}) || !bytes.Equal(s.Index[:8], []byte{255, 't', 'O', 'c', 0, 0, 0, 2}) {
		return nil, ErrUnsafe
	}
	packHash := sha1.Sum(s.Pack[:len(s.Pack)-20])
	indexHash := sha1.Sum(s.Index[:len(s.Index)-20])
	if hex.EncodeToString(packHash[:]) != s.PackName || !bytes.Equal(packHash[:], s.Pack[len(s.Pack)-20:]) || !bytes.Equal(packHash[:], s.Index[len(s.Index)-40:len(s.Index)-20]) || !bytes.Equal(indexHash[:], s.Index[len(s.Index)-20:]) {
		return nil, ErrUnsafe
	}
	return []File{{".git/objects/pack/pack-" + s.PackName + ".pack", s.Pack, false, 0, 0}, {".git/objects/pack/pack-" + s.PackName + ".idx", s.Index, false, 0, 0}, {".git/" + s.Ref, []byte(s.SHA + "\n"), false, 0, 0}}, nil
}
func hexString(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Delta drains a bounded workspace archive while all writers are stopped. It
// rejects indirection and history substitution, checks every destination and
// ancestor, and returns only absent immutable files. Existing bytes are never
// overwritten (including hardlinks to files outside the checkout).
func Delta(input io.Reader, snapshot Snapshot) ([]File, error) {
	files, err := Files(snapshot)
	if err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: input, N: MaxArchiveBytes + 1}
	reader := tar.NewReader(limited)
	seen := map[string]byte{}
	matches := map[string]bool{}
	uid, gid := 0, 0
	for count := 0; ; count++ {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil || count > 200000 {
			return nil, ErrUnsafe
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "workspace" {
			name = ""
		} else {
			name = strings.TrimPrefix(name, "workspace/")
		}
		if name == "" {
			if h.Typeflag != tar.TypeDir {
				return nil, ErrUnsafe
			}
			continue
		}
		if path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			return nil, ErrUnsafe
		}
		if name != ".git" && !strings.HasPrefix(name, ".git/") {
			continue
		}
		if _, ok := seen[name]; ok {
			return nil, ErrUnsafe
		}
		seen[name] = h.Typeflag
		if name == ".git" {
			uid, gid = h.Uid, h.Gid
		}
		// Symlinks, hardlinks, special files and split/common repositories are not
		// supported. No credentialed command ever consults this metadata.
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return nil, ErrUnsafe
		}
		if strings.HasSuffix(name, ".lock") || name == ".git/commondir" || name == ".git/gitdir" || name == ".git/shallow" || name == ".git/reftable" || strings.HasPrefix(name, ".git/reftable/") || name == ".git/info/grafts" || name == ".git/objects/info/alternates" || name == ".git/objects/info/http-alternates" || name == ".git/refs/replace" || strings.HasPrefix(name, ".git/refs/replace/") {
			return nil, ErrUnsafe
		}
		if name == ".git/config" {
			if h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return nil, ErrUnsafe
			}
			config, err := io.ReadAll(reader)
			config = bytes.TrimPrefix(config, []byte{0xef, 0xbb, 0xbf})
			config = bytes.ReplaceAll(config, []byte("\\\n"), nil)
			if err != nil || unsupportedGitSection.Match(config) {
				return nil, ErrUnsafe
			}
		}
		if name == ".git/packed-refs" {
			if h.Size > 16<<20 {
				return nil, ErrUnsafe
			}
			b, err := io.ReadAll(reader)
			if err != nil || bytes.Contains(b, []byte(snapshot.Ref)) || bytes.Contains(b, []byte("refs/replace/")) {
				return nil, ErrUnsafe
			}
		}
		for _, f := range files {
			if name == f.Name {
				if h.Typeflag != tar.TypeReg || h.Size != int64(len(f.Content)) {
					return nil, ErrUnsafe
				}
				hash := sha256.New()
				if _, err := io.Copy(hash, reader); err != nil {
					return nil, ErrUnsafe
				}
				want := sha256.Sum256(f.Content)
				if !bytes.Equal(hash.Sum(nil), want[:]) {
					return nil, ErrUnsafe
				}
				matches[name] = true
			}
		}
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, ErrUnsafe
	}
	if limited.N <= 0 {
		return nil, ErrUnsafe
	}
	for _, required := range []string{".git", ".git/objects", ".git/refs"} {
		if seen[required] != tar.TypeDir {
			return nil, ErrUnsafe
		}
	}
	var delta []File
	addedDirs := map[string]bool{}
	for _, f := range files {
		for parent := path.Dir(f.Name); parent != "."; parent = path.Dir(parent) {
			if kind, ok := seen[parent]; ok && kind != tar.TypeDir {
				return nil, ErrUnsafe
			}
		}
		if !matches[f.Name] {
			var parents []string
			for p := path.Dir(f.Name); p != "."; p = path.Dir(p) {
				parents = append(parents, p)
			}
			for i := len(parents) - 1; i >= 0; i-- {
				p := parents[i]
				if _, ok := seen[p]; !ok && !addedDirs[p] {
					delta = append(delta, File{Name: p, Directory: true, UID: uid, GID: gid})
					addedDirs[p] = true
				}
			}
			f.UID, f.GID = uid, gid
			delta = append(delta, f)
		}
	}
	return delta, nil
}

// Archive emits only validated additive paths and necessary directories.
func Archive(files []File) (io.ReadCloser, error) {
	reader, output := io.Pipe()
	go func() {
		writer := tar.NewWriter(output)
		err := func() error {
			for _, f := range files {
				if f.Directory {
					if err := writer.WriteHeader(&tar.Header{Name: f.Name + "/", Typeflag: tar.TypeDir, Mode: 0755, Uid: f.UID, Gid: f.GID}); err != nil {
						return err
					}
					continue
				}
				if err := writer.WriteHeader(&tar.Header{Name: f.Name, Typeflag: tar.TypeReg, Mode: 0444, Size: int64(len(f.Content)), Uid: f.UID, Gid: f.GID}); err != nil {
					return err
				}
				if _, err := writer.Write(f.Content); err != nil {
					return err
				}
			}
			return writer.Close()
		}()
		_ = output.CloseWithError(err)
	}()
	return reader, nil
}
