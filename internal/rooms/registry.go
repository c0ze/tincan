// Package rooms records the project rooms tincan has been used in, so the web
// UI can list them. The registry is advisory: failures never fail a command.
package rooms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

type Room struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Hidden   bool      `json:"hidden,omitempty"`
	LastUsed time.Time `json:"last_used"`
	Missing  bool      `json:"missing,omitempty"` // computed by List, never stored
}

type file struct {
	Rooms []Room `json:"rooms"`
}

// StateDir is where per-user tincan state lives; "" when it cannot be found.
func StateDir() string {
	if d := os.Getenv("TINCAN_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tincan")
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LocalAppData"); d != "" {
			return filepath.Join(d, "tincan")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "tincan")
}

type Registry struct{ path string }

// Open returns the registry backed by the JSON file at path (state dir may not
// exist yet). path == "" disables the registry: every method is then a no-op.
func Open(path string) *Registry {
	if path == "" {
		return &Registry{}
	}
	return &Registry{path: resolveAncestor(path)}
}

// resolveAncestor resolves symlinks in the longest existing prefix of path,
// leaving components that do not exist yet untouched. This also handles
// /var -> /private/var on macOS for state directories not yet created, the
// same way spool.Open resolves a room directory that already exists.
func resolveAncestor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	suffix, dir := "", abs
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			if suffix == "" {
				return resolved
			}
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		if suffix == "" {
			suffix = filepath.Base(dir)
		} else {
			suffix = filepath.Join(filepath.Base(dir), suffix)
		}
		dir = parent
	}
}

func Default() *Registry {
	d := StateDir()
	if d == "" {
		return Open("")
	}
	return Open(filepath.Join(d, "rooms.json"))
}

func ID(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:12]
}

func Canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// TooBroad reports whether a canonical directory is a filesystem root, the
// home directory, or one of its ancestors: never a project room.
func TooBroad(dir string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return false
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	rel, err := filepath.Rel(dir, home)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// excluded reports paths never registered: phase 2 review workspaces under
// <state dir>/reviews/.
func excluded(path string) bool {
	d := StateDir()
	if d == "" {
		return false
	}
	if c, err := Canonical(d); err == nil {
		d = c
	}
	rel, err := filepath.Rel(filepath.Join(d, "reviews"), path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TouchQuiet records room in the default registry, reporting (not returning)
// any failure.
func TouchQuiet(room string, stderr io.Writer) {
	if err := Default().Touch(room); err != nil {
		fmt.Fprintf(stderr, "tincan: room registry: %v\n", err)
	}
}

func (r *Registry) read() (file, error) {
	var f file
	data, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", r.path, err)
	}
	return f, nil
}

func (r *Registry) update(fn func(*file) error) error {
	if err := fsutil.MkdirPrivate(filepath.Dir(r.path)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := filelock.Acquire(ctx, r.path+".lock")
	if err != nil {
		return err
	}
	defer l.Close()
	f, err := r.read()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(r.path, append(data, '\n'))
}

// upsert is the single insertion point, so the exclusion applies to Touch,
// Add and Import alike.
func upsert(f *file, path string, used time.Time) {
	if excluded(path) {
		return
	}
	id := ID(path)
	for i := range f.Rooms {
		if f.Rooms[i].ID == id {
			if used.After(f.Rooms[i].LastUsed) {
				f.Rooms[i].LastUsed = used
			}
			return
		}
	}
	f.Rooms = append(f.Rooms, Room{ID: id, Path: path, LastUsed: used})
}

func (r *Registry) Touch(room string) error {
	if r.path == "" {
		return nil
	}
	path, err := Canonical(room)
	if err != nil {
		return err
	}
	return r.update(func(f *file) error { upsert(f, path, time.Now().UTC()); return nil })
}

func (r *Registry) Add(dir string) (Room, error) {
	path, err := Canonical(dir)
	if err != nil {
		return Room{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Room{}, err
	}
	if !info.IsDir() {
		return Room{}, fmt.Errorf("%s is not a directory", path)
	}
	if TooBroad(path) {
		return Room{}, fmt.Errorf("%s is the home directory, an ancestor of it, or a filesystem root", path)
	}
	if excluded(path) {
		return Room{}, fmt.Errorf("%s is a review workspace, not a room", path)
	}
	if r.path == "" {
		return Room{}, errors.New("no state directory for the room registry")
	}
	if err := r.update(func(f *file) error { upsert(f, path, time.Now().UTC()); return nil }); err != nil {
		return Room{}, err
	}
	got, _, err := r.Get(ID(path))
	return got, err
}

func (r *Registry) SetHidden(id string, hidden bool) error {
	return r.update(func(f *file) error {
		for i := range f.Rooms {
			if f.Rooms[i].ID == id {
				f.Rooms[i].Hidden = hidden
				return nil
			}
		}
		return fmt.Errorf("unknown room %q", id)
	})
}

func (r *Registry) List() ([]Room, error) {
	if r.path == "" {
		return nil, nil
	}
	f, err := r.read()
	if err != nil {
		return nil, err
	}
	out := append([]Room(nil), f.Rooms...)
	counts := map[string]int{}
	for _, room := range out {
		counts[filepath.Base(room.Path)]++
	}
	for i := range out {
		base := filepath.Base(out[i].Path)
		out[i].Name = base
		if counts[base] > 1 {
			out[i].Name = filepath.Base(filepath.Dir(out[i].Path)) + "/" + base
		}
		if info, err := os.Stat(out[i].Path); err != nil || !info.IsDir() {
			out[i].Missing = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *Registry) Get(id string) (Room, bool, error) {
	list, err := r.List()
	if err != nil {
		return Room{}, false, err
	}
	for _, room := range list {
		if room.ID == id {
			return room, true, nil
		}
	}
	return Room{}, false, nil
}

// Import registers every directory containing a .tincan directory at most
// depth levels below each of dirs. node_modules and dot-directories are skipped.
func (r *Registry) Import(dirs []string, depth int) (int, error) {
	var found []string
	for _, dir := range dirs {
		root, err := Canonical(dir)
		if err != nil {
			continue
		}
		base := strings.Count(root, string(filepath.Separator))
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			name := d.Name()
			if p != root && (name == "node_modules" || (strings.HasPrefix(name, ".") && name != ".tincan")) {
				return fs.SkipDir
			}
			if name == ".tincan" {
				parent := filepath.Dir(p)
				if !TooBroad(parent) {
					found = append(found, parent)
				}
				return fs.SkipDir
			}
			if strings.Count(p, string(filepath.Separator))-base >= depth {
				return fs.SkipDir
			}
			return nil
		})
	}
	if len(found) == 0 || r.path == "" {
		return 0, nil
	}
	added := 0
	err := r.update(func(f *file) error {
		for _, p := range found {
			before := len(f.Rooms)
			info, _ := os.Stat(filepath.Join(p, ".tincan"))
			used := time.Time{}
			if info != nil {
				used = info.ModTime().UTC()
			}
			upsert(f, p, used)
			if len(f.Rooms) > before {
				added++
			}
		}
		return nil
	})
	return added, err
}
