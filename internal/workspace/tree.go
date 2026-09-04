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

func (snapshot TreeSnapshot) Equal(other TreeSnapshot) bool {
	return slices.Equal(snapshot.entries, other.entries)
}

func SyncTree(source, destination string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source tree: %w", err)
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve destination tree: %w", err)
	}
	if pathsOverlap(source, destination) {
		return ErrOverlappingPaths
	}
	sourceDirectory, source, err := openDirectoryNoFollow(source)
	if err != nil {
		return fmt.Errorf("inspect source tree: %w", err)
	}
	defer sourceDirectory.Close()
	destinationInfo, err := os.Lstat(destination)
	if err == nil && !destinationInfo.IsDir() {
		return fmt.Errorf("%w: destination root is not a directory", ErrUnsupportedTreeEntry)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect destination tree: %w", err)
	}
	if os.IsNotExist(err) {
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return fmt.Errorf("create destination tree: %w", err)
		}
	}
	children, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read destination tree: %w", err)
	}
	for _, child := range children {
		if child.Name() == ".git" {
			gitInfo, err := os.Lstat(filepath.Join(destination, child.Name()))
			if err != nil {
				return fmt.Errorf("inspect destination Git metadata: %w", err)
			}
			if !gitInfo.IsDir() {
				return fmt.Errorf("%w: destination .git is not a directory", ErrUnsupportedTreeEntry)
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(destination, child.Name())); err != nil {
			return fmt.Errorf("remove stale destination entry: %w", err)
		}
	}

	var directories []directoryMode
	err = walkSourceTree(sourceDirectory, source, "", "sync", func(entry sourceTreeEntry) error {
		target := filepath.Join(destination, filepath.FromSlash(entry.path))
		switch entry.kind {
		case 'd':
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			directories = append(directories, directoryMode{path: target, mode: entry.mode})
			return nil
		case 'f':
			return copyRegularFile(entry.file, target, entry.mode)
		case 'l':
			return os.Symlink(entry.linkTarget, target)
		default:
			return fmt.Errorf("%w: %s", ErrUnsupportedTreeEntry, entry.path)
		}
	})
	if err != nil {
		return fmt.Errorf("copy workspace tree: %w", err)
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Chmod(directories[index].path, directories[index].mode); err != nil {
			return fmt.Errorf("preserve directory mode: %w", err)
		}
	}
	return nil
}

type directoryMode struct {
	path string
	mode os.FileMode
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

func copyRegularFile(input *os.File, destination string, mode os.FileMode) (err error) {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := output.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err = io.Copy(output, input); err != nil {
		return err
	}
	return os.Chmod(destination, mode)
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
