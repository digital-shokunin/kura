package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// State is the per-clone, per-remote record kept in $GIT_DIR/kura/<remote>/.
// It holds the high-water mark (the newest manifest this clone has verified)
// and which packs have been indexed locally. It is guarded by an flock held
// for the lifetime of the State.
type State struct {
	RepoID  string   `json:"repo_id"`
	Seq     uint64   `json:"seq"`
	Hash    string   `json:"hash"`
	Applied []string `json:"applied,omitempty"`

	dir  string
	lock *os.File
}

// OpenState locks dir and loads state.json from it, creating dir if needed.
func OpenState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flockWithTimeout(lock, dir, lockTimeout); err != nil {
		lock.Close()
		return nil, err
	}
	s := &State{dir: dir, lock: lock}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		s.Close()
		return nil, err
	default:
		if err := json.Unmarshal(b, s); err != nil {
			s.Close()
			return nil, fmt.Errorf("remote: corrupt %s/state.json: %w", dir, err)
		}
	}
	return s, nil
}

const lockTimeout = 2 * time.Minute

func flockWithTimeout(f *os.File, dir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	warned := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("remote: locking %s: %w", dir, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("remote: another kura process has held %s for over %s; "+
				"if none is running, remove %s/lock", dir, timeout, dir)
		}
		if !warned {
			fmt.Fprintf(os.Stderr, "kura: waiting for another kura process using %s\n", dir)
			warned = true
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Close releases the lock.
func (s *State) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// Advance raises the high-water mark. It never moves backwards.
func (s *State) Advance(repoID string, seq uint64, hash string) error {
	if s.Hash != "" && s.RepoID == repoID && seq < s.Seq {
		return nil
	}
	if s.RepoID != repoID {
		s.Applied = nil
	}
	s.RepoID, s.Seq, s.Hash = repoID, seq, hash
	return s.save()
}

// IsApplied reports whether pack id's objects were indexed into this clone.
func (s *State) IsApplied(id string) bool { return slices.Contains(s.Applied, id) }

// MarkApplied records that pack id was indexed into this clone.
func (s *State) MarkApplied(ids ...string) error {
	for _, id := range ids {
		if !s.IsApplied(id) {
			s.Applied = append(s.Applied, id)
		}
	}
	return s.save()
}

// Prune drops applied entries for packs no longer in the manifest.
func (s *State) Prune(m *Manifest) error {
	kept := s.Applied[:0]
	for _, id := range s.Applied {
		if m.HasPack(id) {
			kept = append(kept, id)
		}
	}
	s.Applied = kept
	return s.save()
}

func (s *State) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(s.dir, "state.json"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
