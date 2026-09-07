package request

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/internal/fsutil"
)

const MaxProgressBytes = 4 * 1024 * 1024

type Event struct {
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	Time   time.Time `json:"time"`
}

func progressPath(room, id string) string { return filepath.Join(Dir(room), id+".jsonl") }

// AppendProgress caps disk usage per request. It is safe for simultaneous stdout
// and stderr writers; progress is advisory and cannot replace a terminal result.
func AppendProgress(room, id, stream string, data []byte) error {
	l, err := lock(context.Background(), room, id)
	if err != nil {
		return err
	}
	defer l.Close()
	f, err := fsutil.OpenFile(progressPath(room, id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("progress path is not a regular file")
	}
	remaining := int64(MaxProgressBytes) - info.Size()
	if remaining <= 256 {
		return nil
	}
	for len(data) > 0 {
		// Even worst-case JSON escaping keeps an 8 KiB chunk below the
		// reader's 128 KiB snapshot, so its cursor always reaches a newline.
		n := len(data)
		if n > 8*1024 {
			n = 8 * 1024
		}
		line, err := json.Marshal(Event{Stream: stream, Text: string(data[:n]), Time: time.Now().UTC()})
		if err != nil {
			return err
		}
		if int64(len(line)+1) > remaining {
			return nil
		}
		if _, err = f.Write(append(line, '\n')); err != nil {
			return err
		}
		remaining -= int64(len(line) + 1)
		data = data[n:]
	}
	return nil
}

// Progress returns complete event lines starting at a byte cursor. A bounded
// snapshot keeps slow clients from holding up the host or loading entire logs.
func Progress(room, id string, offset int64, limit int) ([]Event, int64, error) {
	if err := validID(id); err != nil {
		return nil, offset, err
	}
	if offset < 0 || offset > MaxProgressBytes {
		return nil, offset, errors.New("invalid progress cursor")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if err := fsutil.CheckDir(Dir(room)); errors.Is(err, os.ErrNotExist) {
		return []Event{}, offset, nil
	} else if err != nil {
		return nil, offset, err
	}
	f, err := fsutil.OpenFile(progressPath(room, id), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return []Event{}, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if !info.Mode().IsRegular() {
		return nil, offset, errors.New("progress path is not a regular file")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 128*1024))
	if err != nil {
		return nil, offset, err
	}
	events := []Event{}
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		var event Event
		if err := json.Unmarshal(data[start:i], &event); err != nil {
			return nil, offset, err
		}
		events = append(events, event)
		offset += int64(i - start + 1)
		start = i + 1
		if len(events) >= limit {
			break
		}
	}
	return events, offset, nil
}
