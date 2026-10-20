package providerauth

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type credentialStore struct {
	root *os.Root
	lock *os.File
}

func openCredentialStore(directory string) (*credentialStore, savedState, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, savedState{}, ErrStorage
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, savedState{}, ErrStorage
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, savedState{}, ErrStorage
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, savedState{}, ErrStorage
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, savedState{}, ErrStorage
	}
	lock, err := root.OpenFile("writer.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		root.Close()
		return nil, savedState{}, ErrStorage
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		root.Close()
		return nil, savedState{}, ErrStorage
	}
	s := &credentialStore{root: root, lock: lock}
	f, err := root.Open("chatgpt.json")
	if errors.Is(err, os.ErrNotExist) {
		return s, savedState{}, nil
	}
	if err != nil {
		s.Close()
		return nil, savedState{}, ErrStorage
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 256<<10 {
		s.Close()
		return nil, savedState{}, ErrStorage
	}
	var state savedState
	d := json.NewDecoder(io.LimitReader(f, (256<<10)+1))
	d.DisallowUnknownFields()
	if d.Decode(&state) != nil || d.Decode(&struct{}{}) != io.EOF || state.Version != 1 ||
		!strings.HasPrefix(state.HostID, "urn:uuid:") || uuid.Validate(strings.TrimPrefix(state.HostID, "urn:uuid:")) != nil || uuid.Validate(state.Generation) != nil {
		s.Close()
		return nil, savedState{}, ErrStorage
	}
	if c := state.Credential; c != nil && (c.ClientID == "" || c.Subject == "" || !tokenString(c.Access) || !tokenString(c.Refresh) || c.ExpiresAt.IsZero()) {
		s.Close()
		return nil, savedState{}, ErrStorage
	}
	return s, state, nil
}

func (s *credentialStore) Close() error {
	return errors.Join(s.lock.Close(), s.root.Close())
}

func (s *credentialStore) save(state savedState) error {
	content, err := json.Marshal(state)
	if err != nil || len(content) > 256<<10 {
		return ErrStorage
	}
	name := ".chatgpt-" + uuid.NewString()
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrStorage
	}
	defer s.root.Remove(name)
	_, writeErr := f.Write(content)
	syncErr := f.Sync()
	closeErr := f.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		return ErrStorage
	}
	if s.root.Rename(name, "chatgpt.json") != nil {
		return ErrStorage
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return ErrStorage
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return ErrStorage
	}
	return nil
}
