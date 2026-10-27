package gitdelivery

import (
	"archive/tar"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func testSnapshot() Snapshot {
	pack := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	sum := sha1.Sum(pack)
	pack = append(pack, sum[:]...)
	index := make([]byte, 1032)
	copy(index, []byte{255, 't', 'O', 'c', 0, 0, 0, 2})
	index = append(index, sum[:]...)
	idxsum := sha1.Sum(index)
	index = append(index, idxsum[:]...)
	return Snapshot{SHA: strings.Repeat("a", 40), Ref: "refs/omnigrex/default/" + strings.Repeat("b", 64), PackName: hex.EncodeToString(sum[:]), Pack: pack, Index: index}
}
func fixtureArchive(t *testing.T, extra ...*tar.Header) io.Reader {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, p := range []string{"workspace", "workspace/.git", "workspace/.git/objects", "workspace/.git/refs"} {
		if err := w.WriteHeader(&tar.Header{Name: p + "/", Typeflag: tar.TypeDir, Mode: 0700, Uid: 10001, Gid: 10001}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write(make([]byte, h.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}
func TestDeltaOnlyAddsAbsentGitData(t *testing.T) {
	snapshot := testSnapshot()
	files, err := Delta(fixtureArchive(t), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.UID != 10001 || f.GID != 10001 {
			t.Fatal("delivery would break workspace ownership", f.Name)
		}
		if f.Directory && (f.Name == ".git" || f.Name == ".git/objects" || f.Name == ".git/refs") {
			t.Fatalf("rewrites existing directory %s", f.Name)
		}
	}
	archive, err := Archive(files)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	r := tar.NewReader(archive)
	got := map[string]bool{}
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[strings.TrimSuffix(h.Name, "/")] = true
	}
	for _, name := range []string{".git/objects/pack/pack-" + snapshot.PackName + ".pack", ".git/objects/pack/pack-" + snapshot.PackName + ".idx", ".git/" + snapshot.Ref} {
		if !got[name] {
			t.Errorf("missing %s", name)
		}
	}
}
func TestDeltaRejectsHostileMetadata(t *testing.T) {
	for _, name := range []string{".git/objects/info/alternates", ".git/objects/info/http-alternates", ".git/commondir", ".git/shallow", ".git/info/grafts", ".git/refs/replace/abc", ".git/index.lock"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Delta(fixtureArchive(t, &tar.Header{Name: "workspace/" + name, Typeflag: tar.TypeReg}), testSnapshot()); err == nil {
				t.Fatal("accepted history indirection or active writer")
			}
		})
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo} {
		t.Run(string(kind), func(t *testing.T) {
			if _, err := Delta(fixtureArchive(t, &tar.Header{Name: "workspace/.git/objects/pack", Typeflag: kind, Linkname: "/outside"}), testSnapshot()); err == nil {
				t.Fatal("accepted linked destination")
			}
		})
	}
	snapshot := testSnapshot()
	for _, name := range []string{".git/objects/pack/pack-" + snapshot.PackName + ".pack", ".git/refs/omnigrex"} {
		if _, err := Delta(fixtureArchive(t, &tar.Header{Name: "workspace/" + name, Typeflag: tar.TypeReg, Size: 1}), snapshot); err == nil {
			t.Fatal("accepted conflicting existing destination")
		}
	}
}

func TestSnapshotRejectsCorruptPackAndIndex(t *testing.T) {
	for _, part := range []string{"pack", "index", "name"} {
		t.Run(part, func(t *testing.T) {
			s := testSnapshot()
			switch part {
			case "pack":
				s.Pack[0] ^= 1
			case "index":
				s.Index[0] ^= 1
			case "name":
				s.PackName = strings.Repeat("f", 40)
			}
			if _, err := Files(s); err == nil {
				t.Fatal("corrupt transfer accepted")
			}
		})
	}
}

func TestDeltaReplayAndInterruptedInstallationAreFailClosed(t *testing.T) {
	snapshot := testSnapshot()
	files, err := Files(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	archive := func(change int) io.Reader {
		t.Helper()
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for _, p := range []string{".git", ".git/objects", ".git/objects/pack", ".git/refs", ".git/refs/omnigrex", ".git/refs/omnigrex/default"} {
			if err := w.WriteHeader(&tar.Header{Name: p + "/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
				t.Fatal(err)
			}
		}
		for i, f := range files {
			content := f.Content
			if i == change {
				content = content[:len(content)-1]
			}
			if err := w.WriteHeader(&tar.Header{Name: f.Name, Typeflag: tar.TypeReg, Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
			w.Write(content)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return &b
	}
	if delta, err := Delta(archive(-1), snapshot); err != nil || len(delta) != 0 {
		t.Fatalf("idempotent install=%v %v", delta, err)
	}
	for i := range files {
		if _, err := Delta(archive(i), snapshot); err == nil {
			t.Fatal("partial installation accepted", i)
		}
	}
}
