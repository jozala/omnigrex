package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	ErrInvalidAssignmentID  = errors.New("invalid Agent Assignment ID")
	ErrInvalidOptions       = errors.New("invalid workspace lifecycle options")
	ErrOverlappingPaths     = errors.New("assignment paths overlap")
	ErrUnsafeAssignmentPath = errors.New("unsafe Agent Assignment path")
)

var directoryUID = func(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

type Options struct {
	WorkspaceRoot   string
	PublicationRoot string
	MiseRoot        string
	GitExecutable   string
	MiseExecutable  string
}

type Paths struct {
	Workspace   string
	Publication string
	Mise        string
}

type Lifecycle struct {
	workspaceRoot    string
	publicationRoot  string
	miseRoot         string
	gitExecutable    string
	miseExecutable   string
	locksMu          sync.Mutex
	publicationLocks map[string]*publicationLockEntry
}

type publicationLockEntry struct {
	mutex sync.Mutex
	refs  int
}

func New(options Options) (*Lifecycle, error) {
	workspaceRoot, err := normalizeRoot("workspace", options.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	publicationRoot, err := normalizeRoot("publication", options.PublicationRoot)
	if err != nil {
		return nil, err
	}
	miseRoot, err := normalizeRoot("mise", options.MiseRoot)
	if err != nil {
		return nil, err
	}
	gitExecutable := options.GitExecutable
	if gitExecutable == "" {
		gitExecutable = "git"
	}
	miseExecutable := options.MiseExecutable
	if miseExecutable == "" {
		miseExecutable = "mise"
	}
	return &Lifecycle{
		workspaceRoot: workspaceRoot, publicationRoot: publicationRoot, miseRoot: miseRoot,
		gitExecutable: gitExecutable, miseExecutable: miseExecutable,
		publicationLocks: make(map[string]*publicationLockEntry),
	}, nil
}

func (lifecycle *Lifecycle) Paths(assignmentID string) (Paths, error) {
	if lifecycle == nil {
		return Paths{}, fmt.Errorf("%w: nil lifecycle", ErrInvalidOptions)
	}
	if !validUUID(assignmentID) {
		return Paths{}, ErrInvalidAssignmentID
	}
	assignmentRoot := "assignment-" + assignmentID
	paths := Paths{
		Workspace:   filepath.Join(lifecycle.workspaceRoot, assignmentRoot, "workspace"),
		Publication: filepath.Join(lifecycle.publicationRoot, assignmentRoot, "publication"),
		Mise:        filepath.Join(lifecycle.miseRoot, assignmentRoot, "mise"),
	}
	values := []string{paths.Workspace, paths.Publication, paths.Mise}
	for index, value := range values {
		for _, other := range values[index+1:] {
			if pathsOverlap(value, other) {
				return Paths{}, ErrOverlappingPaths
			}
		}
	}
	return paths, nil
}

// DiscardWorkspace removes an assignment workspace after validating every owned directory in its path.
func (lifecycle *Lifecycle) DiscardWorkspace(assignmentID string) error {
	paths, err := lifecycle.Paths(assignmentID)
	if err != nil {
		return err
	}
	for _, path := range []string{lifecycle.workspaceRoot, filepath.Dir(paths.Workspace), paths.Workspace} {
		exists, err := inspectOwnedDirectory(path)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
	}
	if err := os.RemoveAll(paths.Workspace); err != nil {
		return fmt.Errorf("discard assignment workspace: %w", err)
	}
	return nil
}

func (lifecycle *Lifecycle) acquirePublicationLock(assignmentID string) func() {
	lifecycle.locksMu.Lock()
	entry := lifecycle.publicationLocks[assignmentID]
	if entry == nil {
		entry = &publicationLockEntry{}
		lifecycle.publicationLocks[assignmentID] = entry
	}
	entry.refs++
	lifecycle.locksMu.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		lifecycle.locksMu.Lock()
		entry.refs--
		if entry.refs == 0 && lifecycle.publicationLocks[assignmentID] == entry {
			delete(lifecycle.publicationLocks, assignmentID)
		}
		lifecycle.locksMu.Unlock()
	}
}

func normalizeRoot(name, value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("%w: %s root must be a clean absolute path", ErrInvalidOptions, name)
	}
	return value, nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func pathsOverlap(first, second string) bool {
	relative, err := filepath.Rel(first, second)
	if err == nil && (relative == "." || relative != ".." && !filepath.IsAbs(relative) && !startsWithParent(relative)) {
		return true
	}
	relative, err = filepath.Rel(second, first)
	return err == nil && (relative == "." || relative != ".." && !filepath.IsAbs(relative) && !startsWithParent(relative))
}

func startsWithParent(value string) bool {
	return value == ".." || len(value) > 3 && value[:3] == ".."+string(filepath.Separator)
}

func ensureAssignmentDirectory(root, assignmentRoot string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if _, err := inspectOwnedDirectory(root); err != nil {
		return err
	}
	return ensureOwnedDirectory(assignmentRoot, 0o755)
}

func ensureOwnedDirectory(path string, mode os.FileMode) error {
	exists, err := inspectOwnedDirectory(path)
	if err != nil || exists {
		return err
	}
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	exists, err = inspectOwnedDirectory(path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: directory was not created: %s", ErrUnsafeAssignmentPath, path)
	}
	return nil
}

func inspectOwnedDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Agent Assignment directory: %w", err)
	}
	uid, hasUID := directoryUID(info)
	if !info.IsDir() || !hasUID || uid != os.Geteuid() {
		return false, fmt.Errorf("%w: %s", ErrUnsafeAssignmentPath, path)
	}
	return true, nil
}
