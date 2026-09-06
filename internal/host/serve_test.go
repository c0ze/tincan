package host

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/spool"
)

// syncBuffer is a bytes.Buffer safe for the race detector: Serve writes the
// log from its goroutine while tests may read it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startServe runs Serve for name "agent" in the background against a fresh
// room. It returns the room, its spool, the log sink, Serve's result channel
// and a cancel func (the SIGTERM stand-in).
func startServe(t *testing.T, p Preset, label string) (string, *spool.Spool, *syncBuffer, <-chan error, context.CancelFunc) {
	t.Helper()
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	room := t.TempDir()
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&p).normalize(); err != nil {
		t.Fatal(err)
	}
	logbuf := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- Serve(ctx, ServeOptions{Room: room, Name: "agent", Label: label, Preset: p, Log: logbuf})
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not exit on cancel during cleanup")
		}
	})
	if !waitFor(func() bool { _, ok, _ := sp.Present("agent"); return ok }, 10*time.Second) {
		t.Fatalf("serve never parked; log:\n%s", logbuf.String())
	}
	return room, sp, logbuf, done, cancel
}

// send drops an ordinary message into agent's inbox, with a fresh reply
// channel when withReply is set, and returns the envelope.
func send(t *testing.T, sp *spool.Spool, body string, withReply bool) *envelope.Envelope {
	t.Helper()
	e := &envelope.Envelope{ID: envelope.NewID(), From: "orch", To: "agent", TS: time.Now().UTC(), Body: body}
	if withReply {
		e.ReplyTo = "r-" + envelope.NewID()
		e.CorrID = e.ReplyTo
	}
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	return e
}

// askVia sends body and waits for the reply on its channel.
func askVia(t *testing.T, sp *spool.Spool, body string) *envelope.Envelope {
	t.Helper()
	e := send(t, sp, body, true)
	reply, err := sp.Recv(e.ReplyTo, 20*time.Second, false)
	if err != nil {
		t.Fatalf("no reply to %s: %v", e.ID, err)
	}
	return reply
}

// stopServe sends a stop control message and waits for Serve to return nil.
func stopServe(t *testing.T, sp *spool.Spool, done <-chan error) {
	t.Helper()
	if err := sp.Send(&envelope.Envelope{ID: envelope.NewID(), From: "orch", To: "agent", TS: time.Now().UTC(), Kind: "stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after stop, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not exit after stop")
	}
}

func echoPreset() Preset { return Preset{Exec: fakeExec("echo", "{body}")} }

func TestServeEchoesBodyAsReplyAndTracksState(t *testing.T) {
	room, sp, _, done, _ := startServe(t, echoPreset(), "fake")
	st, ok, err := ReadState(room, "agent")
	if err != nil || !ok {
		t.Fatalf("state file missing while parked: %v %v", ok, err)
	}
	if st.State != "parked" || st.Preset != "fake" || st.PID != os.Getpid() || st.CurrentID != "" {
		t.Fatalf("state while parked = %+v", st)
	}
	reply := askVia(t, sp, "review PR 56\nline two")
	if reply.Body != "echo: review PR 56\nline two" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	if reply.From != "agent" || reply.CorrID == "" || reply.To != reply.CorrID {
		t.Fatalf("reply envelope = %+v", reply)
	}
	stopServe(t, sp, done)
	if _, ok, _ := ReadState(room, "agent"); ok {
		t.Fatal("state file still present after stop")
	}
	if _, present, _ := sp.Present("agent"); present {
		t.Fatal("presence still reported after stop")
	}
}

func TestServeStdinPreset(t *testing.T) {
	_, sp, _, done, _ := startServe(t, Preset{Exec: fakeExec("stdin"), Stdin: "body"}, "custom")
	reply := askVia(t, sp, "piped\nbody")
	if reply.Body != "stdin: piped\nbody" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	stopServe(t, sp, done)
}

func TestServeReplyFilePreset(t *testing.T) {
	_, sp, logbuf, done, _ := startServe(t, Preset{Exec: fakeExec("outfile", "{out}", "{body}"), Reply: "file"}, "custom")
	reply := askVia(t, sp, "from the file")
	if reply.Body != "file: from the file" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	stopServe(t, sp, done)
	if !strings.Contains(logbuf.String(), "noise on stdout") {
		t.Fatalf("agent stdout not appended to the host log:\n%s", logbuf.String())
	}
}

func TestServeNonZeroExitRepliesError(t *testing.T) {
	_, sp, _, done, _ := startServe(t, Preset{Exec: fakeExec("fail", "{body}")}, "custom")
	reply := askVia(t, sp, "x")
	if !strings.HasPrefix(reply.Body, "ERROR exit=3\nboom: x") {
		t.Fatalf("reply body = %q", reply.Body)
	}
	stopServe(t, sp, done)
}

func TestServeTimeoutRepliesErrorAndKillsAgent(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	_, sp, logbuf, done, _ := startServe(t, Preset{Exec: fakeExec("sleep", "30", pidfile), ExecTimeoutSec: 1}, "custom")
	reply := askVia(t, sp, "x")
	if reply.Body != "ERROR timeout after 1s" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	pid := readPID(t, pidfile)
	if runtime.GOOS != "windows" && !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
		t.Fatalf("agent %d still alive after timeout", pid)
	}
	stopServe(t, sp, done)
	if !strings.Contains(logbuf.String(), "exit=timeout") {
		t.Fatalf("log lacks timeout marker:\n%s", logbuf.String())
	}
}

func TestServeShowsBusyThenCancelKillsInFlightAgent(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	room, sp, _, done, cancel := startServe(t, Preset{Exec: fakeExec("sleep", "30", pidfile)}, "custom")
	e := send(t, sp, "long job", true)
	if !waitFor(func() bool {
		st, ok, _ := ReadState(room, "agent")
		return ok && st.State == "busy" && st.CurrentID == e.ID
	}, 10*time.Second) {
		st, _, _ := ReadState(room, "agent")
		t.Fatalf("state never became busy for %s: %+v", e.ID, st)
	}
	pid := readPID(t, pidfile)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v on cancel, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not exit after cancel")
	}
	if runtime.GOOS != "windows" && spool.ProcessAlive(pid) {
		t.Fatalf("in-flight agent %d survived shutdown", pid)
	}
	if _, ok, _ := ReadState(room, "agent"); ok {
		t.Fatal("state file not removed on cancel")
	}
	reply, err := sp.Recv(e.ReplyTo, 5*time.Second, false)
	if err != nil || !strings.HasPrefix(reply.Body, "ERROR interrupted") {
		t.Fatalf("asker did not get an interrupted reply: %v %+v", err, reply)
	}
}

func TestServeMessageWithoutReplyToIsLoggedOnly(t *testing.T) {
	_, sp, logbuf, done, _ := startServe(t, echoPreset(), "fake")
	send(t, sp, "fire and forget", false)
	// A second, answered message proves the first was fully processed.
	if r := askVia(t, sp, "second"); r.Body != "echo: second" {
		t.Fatalf("second reply = %q", r.Body)
	}
	stopServe(t, sp, done)
	log := logbuf.String()
	if !strings.Contains(log, "echo: fire and forget") || !strings.Contains(log, "no reply_to") {
		t.Fatalf("log should hold the output and the no-reply note:\n%s", log)
	}
}

func TestServeIgnoresUnknownKind(t *testing.T) {
	_, sp, logbuf, done, _ := startServe(t, echoPreset(), "fake")
	if err := sp.Send(&envelope.Envelope{ID: envelope.NewID(), From: "orch", To: "agent", TS: time.Now().UTC(), Kind: "weird", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if r := askVia(t, sp, "still alive?"); r.Body != "echo: still alive?" {
		t.Fatalf("reply after unknown kind = %q", r.Body)
	}
	stopServe(t, sp, done)
	if !strings.Contains(logbuf.String(), "ignored") {
		t.Fatalf("unknown kind not logged as ignored:\n%s", logbuf.String())
	}
}

func TestServeLogHasPerMessageMarkers(t *testing.T) {
	_, sp, logbuf, done, _ := startServe(t, echoPreset(), "fake")
	e := send(t, sp, "marker", true)
	if _, err := sp.Recv(e.ReplyTo, 20*time.Second, false); err != nil {
		t.Fatal(err)
	}
	stopServe(t, sp, done)
	log := logbuf.String()
	if !strings.Contains(log, "=== "+e.ID+" from=orch started=") || !strings.Contains(log, "=== exit=0 duration=") {
		t.Fatalf("log markers missing:\n%s", log)
	}
}

func TestServeRejectsInvalidName(t *testing.T) {
	err := Serve(context.Background(), ServeOptions{Room: t.TempDir(), Name: "../x", Preset: echoPreset()})
	if err == nil {
		t.Fatal("want error for invalid name")
	}
}
