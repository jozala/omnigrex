package workspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrUnsupportedTreeEntry = errors.New("unsupported workspace tree entry")

// treeEntryInspected is a test seam for mutations that happen after classification.
var treeEntryInspected func(operation, path string)

type TreeSnapshot struct {
	entries []treeEntry
}

type treeEntry struct {
	path       string
	kind       byte
	executable bool
	digest     [sha256.Size]byte
}

func SnapshotTree(root string) (TreeSnapshot, error) {
	directory, absoluteRoot, err := openDirectoryNoFollow(root)
	if err != nil {
		return TreeSnapshot{}, fmt.Errorf("inspect tree root: %w", err)
	}
	defer directory.Close()
	entries := make([]treeEntry, 0)
	err = walkSourceTree(directory, absoluteRoot, "", "snapshot", func(source sourceTreeEntry) error {
		if source.kind == 'd' {
			return nil
		}
		entry := treeEntry{path: source.path, kind: source.kind}
		if source.kind == 'f' {
			entry.executable = source.mode&0o111 != 0
			entry.digest, err = digestFile(source.file)
		} else {
			entry.digest = sha256.Sum256([]byte(source.linkTarget))
		}
		if err == nil {
			entries = append(entries, entry)
		}
		return err
	})
	if err != nil {
		return TreeSnapshot{}, fmt.Errorf("snapshot workspace tree: %w", err)
	}
	return TreeSnapshot{entries: entries}, nil
}

// snapshotTrackedTree reads only Git-tracked paths. Ignored caches and other
// local artifacts are not traversed, but tracked files are opened without
// following a substituted directory or regular-file symlink.
func snapshotTrackedTree(root string, paths []string) (TreeSnapshot, error) {
	directory, _, err := openDirectoryNoFollow(root)
	if err != nil {
		return TreeSnapshot{}, err
	}
	defer directory.Close()
	sort.Strings(paths)
	entries := make([]treeEntry, 0, len(paths))
	for _, path := range paths {
		if len(entries) > 0 && entries[len(entries)-1].path == path {
			return TreeSnapshot{}, fmt.Errorf("%w: duplicate tracked path", ErrUnsupportedTreeEntry)
		}
		entry, err := trackedTreeEntry(directory, path)
		if err != nil {
			return TreeSnapshot{}, err
		}
		entries = append(entries, entry)
	}
	return TreeSnapshot{entries: entries}, nil
}

func trackedTreeEntry(root *os.File, path string) (treeEntry, error) {
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || segment == ".git" {
			return treeEntry{}, ErrUnsupportedTreeEntry
		}
	}
	directory := root
	for _, segment := range segments[:len(segments)-1] {
		child, err := openDirectoryAtNoFollow(int(directory.Fd()), segment)
		if directory != root {
			directory.Close()
		}
		if err != nil {
			return treeEntry{}, err
		}
		directory = child
	}
	if directory != root {
		defer directory.Close()
	}
	name := segments[len(segments)-1]
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return treeEntry{}, err
	}
	entry := treeEntry{path: path}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		file, err := openRegularAtNoFollow(int(directory.Fd()), name)
		if err != nil {
			return treeEntry{}, err
		}
		defer file.Close()
		var opened unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
			return treeEntry{}, err
		}
		if opened.Mode&unix.S_IFMT != unix.S_IFREG {
			return treeEntry{}, ErrUnsupportedTreeEntry
		}
		entry.kind = 'f'
		entry.executable = opened.Mode&0o111 != 0
		entry.digest, err = digestFile(file)
		return entry, err
	case unix.S_IFLNK:
		target, err := readlinkAt(int(directory.Fd()), name)
		entry.kind = 'l'
		entry.digest = sha256.Sum256([]byte(target))
		return entry, err
	default:
		return treeEntry{}, ErrUnsupportedTreeEntry
	}
}

func (snapshot TreeSnapshot) Equal(other TreeSnapshot) bool {
	return slices.Equal(snapshot.entries, other.entries)
}

func digestFile(file *os.File) ([sha256.Size]byte, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

type sourceTreeEntry struct {
	path       string
	kind       byte
	mode       os.FileMode
	file       *os.File
	linkTarget string
}

func walkSourceTree(directory *os.File, root, relative, operation string, visit func(sourceTreeEntry) error) error {
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if name == ".git" {
			continue
		}
		entryPath := name
		if relative != "" {
			entryPath = relative + "/" + name
		}
		var classified unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), name, &classified, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if treeEntryInspected != nil {
			treeEntryInspected(operation, filepath.Join(root, filepath.FromSlash(entryPath)))
		}
		switch classified.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			child, err := openDirectoryAtNoFollow(int(directory.Fd()), name)
			if err != nil {
				return err
			}
			var stat unix.Stat_t
			if err := unix.Fstat(int(child.Fd()), &stat); err != nil {
				child.Close()
				return err
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				child.Close()
				return fmt.Errorf("%w: %s changed type", ErrUnsupportedTreeEntry, entryPath)
			}
			if err := visit(sourceTreeEntry{path: entryPath, kind: 'd', mode: os.FileMode(stat.Mode).Perm()}); err != nil {
				child.Close()
				return err
			}
			err = walkSourceTree(child, root, entryPath, operation, visit)
			closeErr := child.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		case unix.S_IFREG:
			file, err := openRegularAtNoFollow(int(directory.Fd()), name)
			if err != nil {
				return err
			}
			var stat unix.Stat_t
			if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
				file.Close()
				return err
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFREG {
				file.Close()
				return fmt.Errorf("%w: %s changed type", ErrUnsupportedTreeEntry, entryPath)
			}
			err = visit(sourceTreeEntry{path: entryPath, kind: 'f', mode: os.FileMode(stat.Mode).Perm(), file: file})
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		case unix.S_IFLNK:
			target, err := readlinkAt(int(directory.Fd()), name)
			if err != nil {
				return err
			}
			if err := visit(sourceTreeEntry{path: entryPath, kind: 'l', linkTarget: target}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %s", ErrUnsupportedTreeEntry, entryPath)
		}
	}
	return nil
}

func openDirectoryNoFollow(path string) (*os.File, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	absolute = filepath.Clean(absolute)
	fd, err := unix.Open(absolute, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), absolute), absolute, nil
}

func openDirectoryAtNoFollow(directoryFD int, name string) (*os.File, error) {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openRegularAtNoFollow(directoryFD int, name string) (*os.File, error) {
	fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readlinkAt(directoryFD int, name string) (string, error) {
	for size := 128; size <= 1<<20; size *= 2 {
		buffer := make([]byte, size)
		length, err := unix.Readlinkat(directoryFD, name, buffer)
		if err != nil {
			return "", err
		}
		if length < len(buffer) {
			return string(buffer[:length]), nil
		}
	}
	return "", fmt.Errorf("%w: symlink target is too long", ErrUnsupportedTreeEntry)
}
