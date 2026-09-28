package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/packet"
)

const ContentLimit = 1 << 20

func Root(room string) string { return filepath.Join(room, ".tincan", "reviews") }

func validID(id string) error {
	if !strings.HasPrefix(id, "rv-") || len(id) != 19 || strings.ContainsAny(id[3:], "./\\") {
		return fmt.Errorf("invalid review id %q", id)
	}
	return nil
}

func Dir(room, id string) string { return filepath.Join(Root(room), id) }

func List(room string) ([]string, error) {
	entries, err := os.ReadDir(Root(room))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && validID(e.Name()) == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func readJSON(path string, limit int64, v any) error {
	data, err := fsutil.ReadFile(path, limit)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data)
}

func ReadInput(room, id string) (Input, error) {
	var in Input
	if err := validID(id); err != nil {
		return in, err
	}
	return in, readJSON(filepath.Join(Dir(room, id), "input.json"), 1<<20, &in)
}

func ReadPacket(room, id string) (*packet.Packet, error) {
	var p packet.Packet
	if err := validID(id); err != nil {
		return nil, err
	}
	return &p, readJSON(filepath.Join(Dir(room, id), "packet.json"), packet.MaxEncoded+(1<<20), &p)
}

func ReadPrompt(room, id string, n int) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	data, err := fsutil.ReadFile(filepath.Join(Dir(room, id), "prompts", strconv.Itoa(n)+".txt"), 256<<10)
	return string(data), err
}

func ReadState(room, id string) (State, error) {
	var st State
	if err := validID(id); err != nil {
		return st, err
	}
	return st, readJSON(filepath.Join(Dir(room, id), "review.json"), 4<<20, &st)
}

func WriteState(room, id string, st State) error {
	if err := validID(id); err != nil {
		return err
	}
	return writeJSON(filepath.Join(Dir(room, id), "review.json"), st)
}

func Lock(ctx context.Context, room, id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(Dir(room, id), "lock"))
}

func resultPath(room, id string, n int) string {
	return filepath.Join(Dir(room, id), "results", strconv.Itoa(n)+".md")
}

// WriteResult records member n's result once; an existing result is kept.
func WriteResult(room, id string, n int, text string) error {
	p := resultPath(room, id, n)
	if _, err := os.Lstat(p); err == nil {
		return nil
	}
	if err := fsutil.MkdirPrivate(filepath.Dir(p)); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(p, []byte(text))
}

func ReadResult(room, id string, n int) (string, bool, error) {
	data, err := fsutil.ReadFile(resultPath(room, id, n), ContentLimit+64)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(data), err == nil, err
}

func ReadBundle(room, id string) (string, bool, error) {
	data, err := fsutil.ReadFile(filepath.Join(Dir(room, id), "bundle.md"), ContentLimit+64)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(data), err == nil, err
}
