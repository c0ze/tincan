package request

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// journaled writes a finished record for a fresh envelope and returns it.
func journaled(t *testing.T, room string) *envelope.Envelope {
	t.Helper()
	e := &envelope.Envelope{ID: envelope.NewID(), From: "orch", To: "agent", TS: time.Now().UTC(), Body: "work"}
	if _, err := Finish(room, e, "done"); err != nil {
		t.Fatal(err)
	}
	return e
}

// rewriteField replaces one top-level field of e's record with raw JSON.
func rewriteField(t *testing.T, room string, e *envelope.Envelope, key, raw string) {
	t.Helper()
	path := recordPath(room, e.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields[key] = json.RawMessage(raw)
	if data, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Record-level failures carry ErrCorruptRecord or ErrIDCollision, with their
// messages and underlying causes kept; shared storage failures carry neither.
func TestRecordErrorsCarrySentinels(t *testing.T) {
	record := func(t *testing.T, mutate func(t *testing.T, room string, e *envelope.Envelope)) error {
		room := canonicalRoom(t.TempDir())
		e := journaled(t, room)
		mutate(t, room, e)
		_, err := Start(room, e)
		return err
	}
	for name, c := range map[string]struct {
		mutate func(t *testing.T, room string, e *envelope.Envelope)
		cause  func(error) bool
		text   string
	}{
		"syntax": {func(t *testing.T, room string, e *envelope.Envelope) {
			os.WriteFile(recordPath(room, e.ID), []byte("{"), 0o600)
		},
			func(err error) bool { var s *json.SyntaxError; return errors.As(err, &s) }, "unexpected end"},
		"unparsable time": {func(t *testing.T, room string, e *envelope.Envelope) { rewriteField(t, room, e, "created", `"bad"`) },
			func(err error) bool { var p *time.ParseError; return errors.As(err, &p) }, "parsing time"},
		// The decoder's error here depends on the Go version: time.Time's own
		// UnmarshalJSON text before Go 1.27, a *json.UnmarshalTypeError from
		// the json/v2-backed decoder since. The record error keeps whichever.
		"non-string time": {func(t *testing.T, room string, e *envelope.Envelope) { rewriteField(t, room, e, "updated", `5`) },
			func(err error) bool {
				want := json.Unmarshal([]byte(`{"updated":5}`), new(Record))
				return want != nil && err.Error() == want.Error()
			}, ""},
		"wrong field type": {func(t *testing.T, room string, e *envelope.Envelope) { rewriteField(t, room, e, "status", `5`) },
			func(err error) bool { var u *json.UnmarshalTypeError; return errors.As(err, &u) }, "status"},
		"mismatched ID": {func(t *testing.T, room string, e *envelope.Envelope) { rewriteField(t, room, e, "id", `"other"`) },
			func(error) bool { return true }, "corrupt request record"},
		"too large": {func(t *testing.T, room string, e *envelope.Envelope) {
			os.Truncate(recordPath(room, e.ID), MaxRecordBytes+1)
		},
			func(err error) bool { return errors.Is(err, fsutil.ErrTooLarge) }, "exceeds size limit"},
	} {
		t.Run(name, func(t *testing.T) {
			err := record(t, c.mutate)
			if !errors.Is(err, ErrCorruptRecord) || errors.Is(err, ErrIDCollision) || !c.cause(err) || !strings.Contains(err.Error(), c.text) {
				t.Fatalf("Start = %v", err)
			}
			if strings.HasPrefix(err.Error(), ErrCorruptRecord.Error()+": ") {
				t.Fatalf("sentinel changed the message: %v", err)
			}
		})
	}

	room := canonicalRoom(t.TempDir())
	e := journaled(t, room)
	other := *e
	other.Body = "different"
	for _, call := range []func() error{
		func() error { _, err := Start(room, &other); return err },
		func() error { _, err := Finish(room, &other, "x"); return err },
	} {
		if err := call(); !errors.Is(err, ErrIDCollision) || errors.Is(err, ErrCorruptRecord) || err.Error() != "request ID collision: "+e.ID {
			t.Fatalf("collision = %v", err)
		}
	}

	if _, err := Get(room, "missing"); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrCorruptRecord) {
		t.Fatalf("missing record = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// An unsafe record file is the record's fault; an unsafe journal or locks
	// directory is shared storage.
	if err := record(t, func(t *testing.T, room string, e *envelope.Envelope) {
		os.Remove(recordPath(room, e.ID))
		os.Symlink(filepath.Join(room, "elsewhere"), recordPath(room, e.ID))
	}); !errors.Is(err, ErrCorruptRecord) || !errors.Is(err, fsutil.ErrUnsafeFile) {
		t.Fatalf("symlinked record = %v", err)
	}
	// A record that cannot be read is an I/O failure, not a corrupt record.
	if os.Geteuid() != 0 {
		if err := record(t, func(t *testing.T, room string, e *envelope.Envelope) {
			os.Chmod(recordPath(room, e.ID), 0)
		}); !errors.Is(err, os.ErrPermission) || errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("unreadable record = %v", err)
		}
	}
	for _, dir := range []string{"locks", ""} {
		room := canonicalRoom(t.TempDir())
		e := journaled(t, room)
		target := filepath.Join(room, "moved")
		path := filepath.Join(Dir(room), dir)
		if err := os.Rename(path, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := Get(room, e.ID); dir == "" && (!errors.Is(err, fsutil.ErrUnsafeFile) || errors.Is(err, ErrCorruptRecord)) {
			t.Fatalf("Get through a symlinked journal = %v", err)
		}
		if _, err := Start(room, e); !errors.Is(err, fsutil.ErrUnsafeFile) || errors.Is(err, ErrCorruptRecord) || errors.Is(err, ErrIDCollision) {
			t.Fatalf("Start with symlinked %q = %v", dir, err)
		}
	}
}
