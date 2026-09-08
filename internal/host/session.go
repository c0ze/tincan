package host

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/spool"
)

// SupportsSessions identifies adapters with tested command and output formats.
func SupportsSessions(label string) bool {
	switch label {
	case "claude", "grok", "agy", "kimi":
		return true
	default:
		return false
	}
}

// WithSession selects a conversation policy without rewriting the executable.
// Custom commands remain stateless unless their configuration or caller opts in
// to a known provider adapter explicitly.
func WithSession(p Preset, label, mode string) (Preset, error) {
	if mode == "" {
		mode = p.Session
	}
	if mode == "" {
		mode = "stateless"
		if SupportsSessions(label) && len(p.Exec) > 0 && strings.TrimSuffix(filepath.Base(p.Exec[0]), ".exe") == label {
			mode = "persistent"
		}
	}
	if mode != "persistent" && mode != "stateless" {
		return Preset{}, fmt.Errorf("session must be persistent or stateless, got %q", mode)
	}
	if mode == "persistent" && !SupportsSessions(label) {
		return Preset{}, fmt.Errorf("preset %q has no supported persistent session adapter", label)
	}
	p.Session = mode
	p.Exec = append([]string(nil), p.Exec...)
	return p, nil
}

type sessionRecord struct {
	Provider string    `json:"provider"`
	Preset   string    `json:"preset"`
	ID       string    `json:"id,omitempty"`
	Ready    bool      `json:"ready"`
	Updated  time.Time `json:"updated"`
}

func sessionPath(room, name string) string {
	return filepath.Join(room, ".tincan", "sessions", name+".json")
}

// SessionRun adapts one invocation. A caller must hold the hosted listener's
// lifetime lock from preparation until completion. Output calls are serialized
// by Run, and Reply must be called after Run returns.
type SessionRun struct {
	Preset Preset
	path   string
	record sessionRecord
	parser sessionParser
}

// PrepareSession loads one room/listener's explicit provider session. It records
// initialization before executing anything, so a crash before receiving a
// session ID cannot silently create a replacement conversation on the next run.
func PrepareSession(room, name, label string, p Preset) (*SessionRun, error) {
	p, err := WithSession(p, label, "")
	if err != nil {
		return nil, err
	}
	s := &SessionRun{Preset: p}
	if p.Session == "stateless" {
		return s, nil
	}
	if err := spool.ValidName(name); err != nil {
		return nil, err
	}
	room, err = canonicalRoom(room)
	if err != nil {
		return nil, err
	}
	argv, err := sessionArgs(p.Exec, label)
	if err != nil {
		return nil, err
	}
	s.path = sessionPath(room, name)
	identity, _ := json.Marshal(struct {
		Exec         []string
		Stdin, Reply string
	}{p.Exec, p.Stdin, p.Reply})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(identity))
	s.record = sessionRecord{Provider: label, Preset: fingerprint}
	data, err := fsutil.ReadFile(s.path, 16<<10)
	resume := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read session: %w", err)
	}
	if resume {
		if err := json.Unmarshal(data, &s.record); err != nil {
			return nil, fmt.Errorf("read session: %w", err)
		}
		if s.record.Provider != label {
			return nil, fmt.Errorf("listener session belongs to %q, not %q; stop and reset the listener first", s.record.Provider, label)
		}
		if s.record.Preset != fingerprint {
			return nil, errors.New("listener preset changed since its session was created; stop and reset the listener explicitly")
		}
		if !s.record.Ready || !validSessionID(label, s.record.ID) {
			return nil, errors.New("previous session initialization was interrupted; inspect the previous run, then stop and reset the listener explicitly")
		}
		flag := "--resume"
		if label == "agy" {
			flag = "--conversation"
		} else if label == "kimi" {
			flag = "--session"
		}
		argv = appendSessionOptions(argv, flag, s.record.ID)
	} else if label == "claude" || label == "grok" {
		id, err := sessionUUID()
		if err != nil {
			return nil, err
		}
		s.record.ID = id
		argv = appendSessionOptions(argv, "--session-id", id)
	}
	s.Preset.Exec, s.Preset.Reply = argv, "stdout"
	s.parser.provider = label
	s.parser.onSession = s.observeID
	if !resume {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func sessionArgs(argv []string, label string) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty agent command")
	}
	out := []string{argv[0]}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			out = append(out, argv[i:]...)
			break
		}
		key := strings.SplitN(arg, "=", 2)[0]
		switch key {
		case "--continue", "-c", "--resume", "-r", "--session-id", "--conversation", "--session", "-S", "--fork-session", "--no-session-persistence":
			return nil, fmt.Errorf("persistent session adapter owns %s; remove that preset argument or use stateless mode", key)
		case "-s":
			if label == "grok" {
				return nil, errors.New("persistent session adapter owns -s; remove that preset argument")
			}
		case "--output-format":
			if arg == key {
				if i+1 == len(argv) {
					return nil, errors.New("--output-format requires a value")
				}
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	format := "stream-json"
	if label == "grok" {
		format = "streaming-json"
	}
	out = appendSessionOptions(out, "--output-format", format)
	if label == "claude" && !hasExactArg(out, "--verbose") {
		out = appendSessionOptions(out, "--verbose")
	}
	return out, nil
}

func hasExactArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

func appendSessionOptions(argv []string, options ...string) []string {
	for i, arg := range argv {
		if arg == "--" {
			out := append([]string(nil), argv[:i]...)
			out = append(out, options...)
			return append(out, argv[i:]...)
		}
	}
	return append(argv, options...)
}

func validSessionID(provider, id string) bool {
	if provider == "kimi" {
		if !strings.HasPrefix(id, "session_") {
			return false
		}
		id = strings.TrimPrefix(id, "session_")
	}
	// Grok accepts titles as --resume values too. UUID validation is necessary
	// to guarantee that a saved pointer selects an ID rather than another
	// listener's similarly named conversation.
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (s *SessionRun) observeID(id string) error {
	if !validSessionID(s.record.Provider, id) {
		return errors.New("provider returned an invalid session ID")
	}
	if s.record.ID != "" && s.record.ID != id {
		return errors.New("provider returned a different session ID; refusing to replace existing conversation")
	}
	if s.record.Ready {
		return nil
	}
	s.record.ID, s.record.Ready = id, true
	return s.save()
}

func (s *SessionRun) save() error {
	s.record.Updated = time.Now().UTC()
	data, err := json.Marshal(s.record)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("persist session: %w", err)
	}
	return nil
}

func sessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// ClearSession forgets the provider pointer, leaving provider transcripts alone.
// The launch lock excludes a new Up and the lifetime lock requires a stopped
// host. A failed reset never alters the existing session.
func ClearSession(ctx context.Context, room, name string) error {
	if err := spool.ValidName(name); err != nil {
		return err
	}
	room, err := canonicalRoom(room)
	if err != nil {
		return err
	}
	launch, err := filelock.Acquire(ctx, launchPath(room, name))
	if err != nil {
		return err
	}
	defer launch.Close()
	lifetime, err := filelock.Try(lifetimePath(room, name))
	if errors.Is(err, filelock.ErrLocked) {
		return errors.New("listener must be stopped before resetting its session")
	}
	if err != nil {
		return err
	}
	defer lifetime.Close()
	path := sessionPath(room, name)
	if _, err := fsutil.ReadFile(path, 16<<10); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil && !errors.Is(err, fsutil.ErrTooLarge) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}
