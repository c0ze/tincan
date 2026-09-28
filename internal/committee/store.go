package committee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

const maxFileBytes = 1 << 20

type storeFile struct {
	Committees []Committee `json:"committees"`
}

// Store is the hub's committees.json in the shared state directory.
type Store struct{ dir string }

func NewStore(stateDir string) *Store { return &Store{dir: stateDir} }

func (s *Store) Path() string { return filepath.Join(s.dir, "committees.json") }

func (s *Store) lockPath() string { return filepath.Join(s.dir, "committees.lock") }

// List returns the committees sorted by name. A missing file is empty; a
// malformed one is an error naming the file, never an empty list.
func (s *Store) List() ([]Committee, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	return f.Committees, nil
}

func (s *Store) read() (storeFile, error) {
	var f storeFile
	data, err := fsutil.ReadFile(s.Path(), maxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return storeFile{Committees: []Committee{}}, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", s.Path(), err)
	}
	if f.Committees == nil {
		f.Committees = []Committee{}
	}
	sort.Slice(f.Committees, func(i, j int) bool { return f.Committees[i].Name < f.Committees[j].Name })
	return f, nil
}

func (s *Store) update(ctx context.Context, fn func(*storeFile) error) error {
	if err := fsutil.MkdirPrivate(s.dir); err != nil {
		return err
	}
	lock, err := filelock.Acquire(ctx, s.lockPath())
	if err != nil {
		return err
	}
	defer lock.Close()
	f, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	sort.Slice(f.Committees, func(i, j int) bool { return f.Committees[i].Name < f.Committees[j].Name })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.Path(), data)
}

// Put creates or replaces the committee named c.Name. The stored version is
// the previous version plus one (1 for a new committee); the caller's
// Version is ignored.
func (s *Store) Put(ctx context.Context, c Committee) (Committee, error) {
	if err := c.Normalize(); err != nil {
		return Committee{}, err
	}
	err := s.update(ctx, func(f *storeFile) error {
		c.Version = 1
		for i, old := range f.Committees {
			if old.Name == c.Name {
				c.Version = old.Version + 1
				f.Committees[i] = c
				return nil
			}
		}
		if len(f.Committees) >= MaxCommittees {
			return fmt.Errorf("at most %d committees", MaxCommittees)
		}
		f.Committees = append(f.Committees, c)
		return nil
	})
	if err != nil {
		return Committee{}, err
	}
	return c, nil
}

// Delete removes the named committee and reports whether it existed.
func (s *Store) Delete(ctx context.Context, name string) (bool, error) {
	found := false
	err := s.update(ctx, func(f *storeFile) error {
		for i, c := range f.Committees {
			if c.Name == name {
				f.Committees = append(f.Committees[:i], f.Committees[i+1:]...)
				found = true
				return nil
			}
		}
		return nil
	})
	return found, err
}

// Cache is a peer's last copy of the hub's committees.
type Cache struct {
	From        string      `json:"from"`
	FetchedAt   time.Time   `json:"fetched_at"`
	AttemptedAt time.Time   `json:"attempted_at"`
	Error       string      `json:"error,omitempty"`
	Committees  []Committee `json:"committees"`
}

func CachePath(stateDir string) string { return filepath.Join(stateDir, "committees-cache.json") }

// LoadCache reads the peer cache; ok is false when there is none yet.
func LoadCache(stateDir string) (Cache, bool, error) {
	var c Cache
	data, err := fsutil.ReadFile(CachePath(stateDir), maxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, false, fmt.Errorf("%s: %w", CachePath(stateDir), err)
	}
	if c.Committees == nil {
		c.Committees = []Committee{}
	}
	return c, true, nil
}

// SaveCache atomically replaces the peer cache.
func SaveCache(stateDir string, c Cache) error {
	if err := fsutil.MkdirPrivate(stateDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(CachePath(stateDir), data)
}

// Lookup finds a committee on this machine: in the peer cache when this
// machine reads committees from a hub, else in the hub store.
func Lookup(stateDir, name string, fromPeer bool) (Committee, error) {
	var list []Committee
	where := "on this machine (the committees hub)"
	if fromPeer {
		c, ok, err := LoadCache(stateDir)
		if err != nil {
			return Committee{}, err
		}
		if !ok {
			return Committee{}, fmt.Errorf("committee %q not found: no committees fetched from the hub yet", name)
		}
		list = c.Committees
		where = fmt.Sprintf("in the copy fetched from %s at %s", c.From, c.FetchedAt.Format(time.RFC3339))
	} else {
		var err error
		if list, err = NewStore(stateDir).List(); err != nil {
			return Committee{}, err
		}
	}
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}
	return Committee{}, fmt.Errorf("committee %q not found %s", name, where)
}

// List returns the committees this machine sees: the peer cache when it
// reads committees from a hub, else the hub store.
func List(stateDir string, fromPeer bool) ([]Committee, error) {
	if !fromPeer {
		return NewStore(stateDir).List()
	}
	c, ok, err := LoadCache(stateDir)
	if err != nil || !ok {
		return nil, err
	}
	return c.Committees, nil
}
