package workspace

import (
	"context"
	"errors"
	"io"
	"math"
	"os"

	"github.com/jozala/omnigrex/internal/uuidtext"
	"golang.org/x/sys/unix"
)

const (
	toolCacheDirBatchSize = 128
	maxToolCacheDepth     = 32
	maxToolCacheHardlinks = 8192
)

type cacheFileIdentity struct{ dev, ino uint64 }

func cacheIdentity(stat unix.Stat_t) cacheFileIdentity {
	return cacheFileIdentity{uint64(stat.Dev), uint64(stat.Ino)}
}

// Each frame retains both the OS directory stream and unread batched entries.
// Depth-first traversal needs no sibling frontier, offsets, or replay sets.
type cacheDirectory struct {
	file     *os.File
	name     string
	identity cacheFileIdentity
	names    []string
}

type toolCacheScan struct {
	root          cacheFileIdentity
	frames        []cacheDirectory
	usage         ToolCacheUsage
	hardlinks     map[cacheFileIdentity]struct{}
	depthLimit    int
	hardlinkLimit int
}

func newToolCacheScan(directory *os.File, stat unix.Stat_t) *toolCacheScan {
	scan := &toolCacheScan{
		root: cacheIdentity(stat), frames: []cacheDirectory{{file: directory, identity: cacheIdentity(stat)}},
		hardlinks:  make(map[cacheFileIdentity]struct{}),
		depthLimit: maxToolCacheDepth, hardlinkLimit: maxToolCacheHardlinks,
	}
	scan.add(stat)
	return scan
}

// Step yields between entries. Budget/deadline exhaustion preserves every handle
// and buffered name; a deadline cannot preempt a blocking filesystem syscall.
func (scan *toolCacheScan) Step(ctx context.Context, budget int) (ToolCacheUsage, error) {
	processed := 0
	for len(scan.frames) > 0 {
		if err := ctx.Err(); err != nil {
			return scan.result(), err
		}
		if err := scan.validateFrames(ctx); err != nil {
			return scan.result(), err
		}
		frame := &scan.frames[len(scan.frames)-1]
		if len(frame.names) == 0 {
			names, err := frame.file.Readdirnames(toolCacheDirBatchSize)
			if err != nil && !errors.Is(err, io.EOF) {
				return scan.result(), err
			}
			frame.names = names
			if len(names) == 0 {
				if err := frame.file.Close(); err != nil {
					return scan.result(), err
				}
				scan.frames = scan.frames[:len(scan.frames)-1]
				continue
			}
		}
		if processed >= budget {
			return scan.result(), nil
		}
		name := frame.names[0]
		frame.names[0] = ""
		frame.names = frame.names[1:]
		processed++ // Every name counts, including vanished entries and symlinks.
		var stat unix.Stat_t
		if err := unix.Fstatat(int(frame.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if !os.IsNotExist(err) {
				scan.usage.Truncated = true
			}
			continue
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if len(scan.frames) >= scan.depthLimit {
				scan.usage.Truncated = true
				continue
			}
			child, opened, err := openCacheDirectoryAt(frame.file, name, scan.root.dev)
			if err != nil {
				scan.usage.Truncated = true
				continue
			}
			scan.add(opened)
			scan.frames = append(scan.frames, cacheDirectory{file: child, name: name, identity: cacheIdentity(opened)})
		case unix.S_IFREG:
			if uint64(stat.Dev) != scan.root.dev {
				scan.usage.Truncated = true
				continue
			}
			if stat.Nlink > 1 {
				identity := cacheIdentity(stat)
				if _, duplicate := scan.hardlinks[identity]; duplicate {
					continue
				}
				if len(scan.hardlinks) >= scan.hardlinkLimit {
					scan.usage.Truncated = true
					continue // Never evict identities and then count aliases again.
				}
				scan.hardlinks[identity] = struct{}{}
			}
			scan.add(stat)
			scan.usage.Files++
		}
	}
	return scan.result(), nil
}

// Retained handles must still belong to the pinned parent chain while traversing.
// A moved/replaced subtree is skipped rather than traversed at its new location.
func (scan *toolCacheScan) validateFrames(ctx context.Context) error {
	for i := 1; i < len(scan.frames); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var stat unix.Stat_t
		frame := scan.frames[i]
		err := unix.Fstatat(int(scan.frames[i-1].file.Fd()), frame.name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || cacheIdentity(stat) != frame.identity {
			scan.usage.Truncated = true
			for _, abandoned := range scan.frames[i:] {
				_ = abandoned.file.Close()
			}
			scan.frames = scan.frames[:i]
			break
		}
	}
	return nil
}

func (scan *toolCacheScan) result() ToolCacheUsage {
	usage := scan.usage
	usage.Truncated = usage.Truncated || len(scan.frames) > 0
	return usage
}

func (scan *toolCacheScan) add(stat unix.Stat_t) {
	if stat.Blocks <= 0 {
		return
	}
	if stat.Blocks > (math.MaxInt64-scan.usage.Bytes)/512 {
		scan.usage.Bytes = math.MaxInt64
	} else {
		scan.usage.Bytes += stat.Blocks * 512
	}
}

func (scan *toolCacheScan) Close() error {
	var err error
	for _, frame := range scan.frames {
		err = errors.Join(err, frame.file.Close())
	}
	scan.frames = nil
	clear(scan.hardlinks)
	return err
}

// Pin every cache-root component relative to its parent, without a path-based
// check/open race or cleanup's permission-restoring helpers.
func (lifecycle *Lifecycle) openToolCacheRoot(id string) (*os.File, unix.Stat_t, error) {
	if lifecycle == nil || !uuidtext.Valid(id) {
		return nil, unix.Stat_t{}, ErrInvalidAssignmentID
	}
	parent, _, err := openDirectoryNoFollow(lifecycle.miseRoot)
	if os.IsNotExist(err) {
		return nil, unix.Stat_t{}, nil
	}
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &stat); err != nil || int(stat.Uid) != os.Geteuid() {
		_ = parent.Close()
		return nil, stat, errors.Join(ErrUnsafeAssignmentPath, err)
	}
	dev := uint64(stat.Dev)
	for _, name := range []string{"assignment-" + id, "tool-data", "assignment"} {
		child, opened, err := openCacheDirectoryAt(parent, name, dev)
		_ = parent.Close()
		if os.IsNotExist(err) {
			return nil, unix.Stat_t{}, nil
		}
		if err != nil {
			return nil, unix.Stat_t{}, err
		}
		parent, stat = child, opened
	}
	return parent, stat, nil
}

func openCacheDirectoryAt(parent *os.File, name string, dev uint64) (*os.File, unix.Stat_t, error) {
	var expected unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &expected, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, expected, err
	}
	if expected.Mode&unix.S_IFMT != unix.S_IFDIR || int(expected.Uid) != os.Geteuid() || uint64(expected.Dev) != dev {
		return nil, expected, ErrUnsafeAssignmentPath
	}
	child, err := openDirectoryAtNoFollow(int(parent.Fd()), name)
	if err != nil {
		return nil, expected, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(child.Fd()), &opened); err != nil || cacheIdentity(opened) != cacheIdentity(expected) || int(opened.Uid) != os.Geteuid() {
		_ = child.Close()
		return nil, opened, errors.Join(ErrUnsafeAssignmentPath, err)
	}
	return child, opened, nil
}
