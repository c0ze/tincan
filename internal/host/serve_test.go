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

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
)

func TestServeRejectsDuplicateAndPreservesOwnerState(t *testing.T) {
	room, sp, _, done, _ := startServe(t, echoPreset(), "fake")
	before, _, _ := ReadState(room, "agent")
	err := Serve(context.Background(), ServeOptions{Room: room, Name: "agent", Preset: echoPreset()})
	if err == nil {
		t.Fatal("duplicate Serve acquired the lifetime")
	}
	after, ok, _ := ReadState(room, "agent")
	if !ok || after.Owner != before.Owner || !after.Alive() {
		t.Fatalf("duplicate clobbered owner: %+v", after)
	}
	if err := RemoveOwnedState(room, "agent", "wrong-owner"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ReadState(room, "agent"); !ok {
		t.Fatal("wrong owner deleted state")
	}
	if err := WriteState(room, "agent", State{Owner: "wrong-owner"}); err == nil {
		t.Fatal("wrong owner overwrote state")
	}
	stopServe(t, sp, done)
}

func TestUpRejectsDifferentExistingConfiguration(t *testing.T) {
	room, sp, _, done, _ := startServe(t, echoPreset(), "fake")
	result, err := Up(context.Background(), UpOptions{Room: room, Name: "agent", Label: "fake", Preset: echoPreset()})
	if err != nil || !result.Already || result.State.Session != "stateless" {
		t.Fatalf("same config reconnect: %+v %v", result, err)
	}
	for _, change := range []func(*Preset){
		func(p *Preset) { p.Stdin = "body" },
		func(p *Preset) { p.ExecTimeoutSec = 1 },
		func(p *Preset) { p.Exec = append(p.Exec, "changed") },
	} {
		p := echoPreset()
		change(&p)
		got, err := Up(context.Background(), UpOptions{Room: room, Name: "agent", Label: "fake", Preset: p})
		if err == nil || !strings.Contains(err.Error(), "different configuration") || got.State.Owner != result.State.Owner {
			t.Fatalf("configuration mismatch not rejected: %+v %v", got, err)
		}
	}
	stopServe(t, sp, done)
}

func TestUpRejectsChangingExistingSessionMode(t *testing.T) {
	room, sp, _, done, _ := startServe(t, echoPreset(), "claude")
	p := echoPreset()
	p.Session = "persistent"
	result, err := Up(context.Background(), UpOptions{Room: room, Name: "agent", Label: "claude", Preset: p})
	if err == nil || !strings.Contains(err.Error(), "different configuration") || result.State.Session != "stateless" {
		t.Fatalf("session mismatch: %+v %v", result, err)
	}
	stopServe(t, sp, done)
}

func TestCancelInterruptsOnlyCurrentRequest(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	room, sp, _, done, _ := startServe(t, Preset{Exec: fakeExec("sleep", "30", pidfile)}, "custom")
	e := send(t, sp, "cancel me", true)
	pid := readPID(t, pidfile)
	if err := Cancel(context.Background(), room, "agent", e.ID); err != nil {
		t.Fatal(err)
	}
	reply, err := sp.Recv(e.ReplyTo, 5*time.Second, false)
	if err != nil || !strings.HasPrefix(reply.Body, "ERROR interrupted") {
		t.Fatalf("cancel reply: %+v %v", reply, err)
	}
	if !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
		t.Fatalf("canceled child %d alive", pid)
	}
	st, ok, _ := ReadState(room, "agent")
	if !ok || !st.Alive() {
		t.Fatal("cancel stopped the listener")
	}
	stopServe(t, sp, done)
}

func TestServeRecoversAbandonedRequestWithoutReplaying(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	room := t.TempDir()
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	e := send(t, sp, "uncertain side effect", true)
	if _, err := sp.ClaimContext(context.Background(), "agent", time.Second); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(room, "should-not-exist")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{Room: room, Name: "agent", Label: "custom", Preset: Preset{Exec: fakeExec("sleep", "30", pidfile)}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("recovery Serve did not stop")
		}
	})
	reply, err := sp.Recv(e.ReplyTo, 5*time.Second, false)
	if err != nil || !strings.Contains(reply.Body, "uncertain") {
		t.Fatalf("recovery reply: %+v %v", reply, err)
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("abandoned side effect was replayed: %v", err)
	}
	r, err := request.Get(room, e.ID)
	if err != nil || !r.Terminal() {
		t.Fatalf("terminal result missing: %+v %v", r, err)
	}
	if !waitFor(func() bool { d, _ := sp.ListInFlight("agent"); return len(d) == 0 }, time.Second) {
		t.Fatal("recovered delivery not acknowledged")
	}
}

func TestServeSkipsCanceledQueuedRequest(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	room := t.TempDir()
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	r, err := request.Submit(context.Background(), room, "agent", "orch", "canceled work", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request.Cancel(context.Background(), room, r.ID); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(room, "should-not-exist")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{Room: room, Name: "agent", Preset: Preset{Exec: fakeExec("sleep", "30", pidfile)}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	if !waitFor(func() bool {
		queued, err := os.ReadDir(sp.InboxDir("agent"))
		if err != nil || len(queued) != 0 {
			return false
		}
		inflight, err := os.ReadDir(filepath.Join(room, ".tincan", "inflight", "agent"))
		return err == nil && len(inflight) == 0
	}, 5*time.Second) {
		t.Fatal("canceled request was not acknowledged")
	}
	reply, err := request.Get(room, r.ID)
	if err != nil || !strings.HasPrefix(reply.Result, "ERROR canceled") {
		t.Fatalf("canceled result: %+v %v", reply, err)
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("canceled work ran: %v", err)
	}
}

func TestServeRecoveryReusesTerminalResultWithReplyAlreadyQueued(t *testing.T) {
	room, sp, _, done, _ := startServe(t, echoPreset(), "fake")
	e := send(t, sp, "execute once", true)
	if !waitFor(func() bool { r, err := request.Get(room, e.ID); return err == nil && r.Terminal() }, 5*time.Second) {
		t.Fatal("initial execution did not finish")
	}
	stopServe(t, sp, done)
	// Recreate a delivery left claimed after the terminal result and reply
	// were durable, as with a crash immediately before Ack.
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.ClaimContext(context.Background(), "agent", time.Second); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(room, "must-not-run")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan error, 1)
	go func() {
		second <- Serve(ctx, ServeOptions{Room: room, Name: "agent", Preset: Preset{Exec: fakeExec("sleep", "30", pidfile)}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-second:
		case <-time.After(5 * time.Second):
			t.Error("second Serve did not stop")
		}
	})
	if !waitFor(func() bool { st, ok, _ := ReadState(room, "agent"); return ok && st.Ready(context.Background()) }, 5*time.Second) {
		t.Fatal("host could not recover an already-queued terminal reply")
	}
	reply, err := sp.Recv(e.ReplyTo, time.Second, false)
	if err != nil || reply.Body != "echo: execute once" {
		t.Fatalf("terminal reply changed: %+v %v", reply, err)
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatalf("terminal request executed again: %v", err)
	}
}

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

func TestServeReplyFileWithSymlinkTemporaryDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges")
	}
	base := t.TempDir()
	target := filepath.Join(base, "actual-temp")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "temp-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, sp, _, done, _ := startServe(t, Preset{Exec: fakeExec("outfile", "{out}", "{body}"), Reply: "file"}, "custom")
	t.Setenv("TMPDIR", link)
	reply := askVia(t, sp, "through a symlink temporary directory")
	if reply.Body != "file: through a symlink temporary directory" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	stopServe(t, sp, done)
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("reply temporary file was not cleaned up: %v %v", entries, err)
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
	if !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
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
	if spool.ProcessAlive(pid) {
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
