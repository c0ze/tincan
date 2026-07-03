package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/spool"
)

// run invokes the CLI and returns (exit code, stdout, stderr).
func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSendThenRecvJSON(t *testing.T) {
	room := t.TempDir()
	code, _, stderr := run("send", "--room", room, "--to", "codex", "--from", "orch",
		"--body", "review PR 56", "--artifact", "a.txt", "--artifact", "b.txt")
	if code != 0 {
		t.Fatalf("send exit %d, stderr=%q", code, stderr)
	}
	code, stdout, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "5")
	if code != 0 {
		t.Fatalf("recv exit %d, stderr=%q", code, stderr)
	}
	e, err := envelope.Unmarshal([]byte(stdout))
	if err != nil {
		t.Fatalf("recv output is not an envelope: %v\n%s", err, stdout)
	}
	if e.Body != "review PR 56" || e.From != "orch" || e.To != "codex" {
		t.Fatalf("wrong envelope: %+v", e)
	}
	if len(e.Artifacts) != 2 || e.Artifacts[0] != "a.txt" || e.Artifacts[1] != "b.txt" {
		t.Fatalf("artifacts not preserved: %+v", e.Artifacts)
	}
}

func TestRecvFormatBody(t *testing.T) {
	room := t.TempDir()
	if code, _, stderr := run("send", "--room", room, "--to", "x", "--from", "y", "--body", "just the text"); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	code, stdout, _ := run("recv", "--room", room, "--as", "x", "--timeout", "5", "--format", "body")
	if code != 0 {
		t.Fatalf("recv exit %d", code)
	}
	if strings.TrimSpace(stdout) != "just the text" {
		t.Fatalf("body format output = %q", stdout)
	}
}

func TestRecvTimeoutExitsThreeSilently(t *testing.T) {
	// Room must be git-free (see gitFreeTempDir): a bare t.TempDir() only
	// guarantees the leaf directory has no .git, not that none of its
	// ancestors do, and roomRootWarning walks ancestors up to the
	// filesystem root. This test's contract is silence on timeout, which
	// roomRootWarning must not disturb when the room is genuinely outside
	// any repo.
	code, stdout, stderr := run("recv", "--room", gitFreeTempDir(t), "--as", "nobody", "--timeout", "1")
	if code != 3 {
		t.Fatalf("want exit 3 on timeout, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("want no stdout on timeout, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("want no stderr on timeout, got %q", stderr)
	}
}

func TestRecvZeroTimeoutBlocksUntilMessageArrives(t *testing.T) {
	room := t.TempDir()
	type recvResult struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan recvResult, 1)
	go func() {
		code, stdout, stderr := run("recv", "--room", room, "--as", "codex",
			"--timeout", "0", "--format", "body")
		done <- recvResult{code, stdout, stderr}
	}()
	go func() {
		time.Sleep(50 * time.Millisecond)
		if code, _, stderr := run("send", "--room", room, "--to", "codex", "--from", "orch",
			"--body", "no re-arm needed"); code != 0 {
			t.Errorf("send failed: exit %d, stderr=%q", code, stderr)
		}
	}()
	select {
	case r := <-done:
		if r.code != 0 {
			t.Fatalf("recv --timeout 0 exit %d, stderr=%q", r.code, r.stderr)
		}
		if strings.TrimSpace(r.stdout) != "no re-arm needed" {
			t.Fatalf("recv --timeout 0 body = %q", r.stdout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recv --timeout 0 hung instead of blocking-then-returning on the delayed send")
	}
}

func TestSendBodyFile(t *testing.T) {
	room := t.TempDir()
	f := room + "/task.md"
	if err := os.WriteFile(f, []byte("long instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("send", "--room", room, "--to", "x", "--from", "y", "--body-file", f); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	_, stdout, _ := run("recv", "--room", room, "--as", "x", "--timeout", "5", "--format", "body")
	if strings.TrimSpace(stdout) != "long instructions" {
		t.Fatalf("got %q", stdout)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"send", "--to", "x"},                // missing --from and body
		{"send", "--to", "x", "--from", "y"}, // missing body
		{"recv"},                             // missing --as
		{"recv", "--as", "x", "--format", "yaml"},                               // bad format
		{"send", "--to", "x", "--from", "y", "--body", "a", "--body-file", "b"}, // both bodies
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("args %v: want exit 2, got %d", args, code)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, stdout, _ := run("help")
	if code != 0 || !strings.Contains(stdout, "tincan") {
		t.Fatalf("help: code=%d out=%q", code, stdout)
	}
}

func TestAskReplyRoundTrip(t *testing.T) {
	room := t.TempDir()
	askDone := make(chan int, 1)
	var askOut bytes.Buffer
	go func() {
		askDone <- Run([]string{"ask", "--room", room, "--to", "codex", "--from", "orch",
			"--body", "2+2?", "--timeout", "30", "--format", "body"}, &askOut, io.Discard)
	}()
	// Listener side: receive the request.
	code, reqJSON, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "30")
	if code != 0 {
		t.Fatalf("listener recv exit %d: %s", code, stderr)
	}
	req, err := envelope.Unmarshal([]byte(reqJSON))
	if err != nil {
		t.Fatalf("bad request json: %v", err)
	}
	if req.ReplyTo == "" || !strings.HasPrefix(req.ReplyTo, "r-") {
		t.Fatalf("request has no r-* reply channel: %+v", req)
	}
	if req.CorrID != req.ReplyTo {
		t.Fatalf("corr_id %q != reply_to %q", req.CorrID, req.ReplyTo)
	}
	// Listener replies.
	if code, _, stderr := run("reply", "--room", room, "--channel", req.ReplyTo,
		"--from", "codex", "--body", "4"); code != 0 {
		t.Fatalf("reply exit %d: %s", code, stderr)
	}
	if code := <-askDone; code != 0 {
		t.Fatalf("ask exit %d", code)
	}
	if strings.TrimSpace(askOut.String()) != "4" {
		t.Fatalf("ask output = %q, want 4", askOut.String())
	}
	// Successful ask must clean up its reply channel.
	if _, err := os.Stat(filepath.Join(room, ".tincan", "inbox", req.ReplyTo)); !os.IsNotExist(err) {
		t.Fatalf("reply channel not cleaned up (err=%v)", err)
	}
}

func TestAskTimeoutPrintsPendingAndLateReplyIsCollectable(t *testing.T) {
	room := t.TempDir()
	code, stdout, _ := run("ask", "--room", room, "--to", "codex", "--from", "orch",
		"--body", "anyone there?", "--timeout", "1")
	if code != 3 {
		t.Fatalf("want exit 3, got %d", code)
	}
	line := strings.TrimSpace(stdout)
	if !strings.HasPrefix(line, "pending channel=r-") {
		t.Fatalf("want 'pending channel=r-...', got %q", line)
	}
	channel := strings.TrimPrefix(line, "pending channel=")
	// The request is still queued for codex.
	code, reqJSON, _ := run("recv", "--room", room, "--as", "codex", "--timeout", "5")
	if code != 0 {
		t.Fatalf("request lost after ask timeout (exit %d)", code)
	}
	req, _ := envelope.Unmarshal([]byte(reqJSON))
	if req.ReplyTo != channel {
		t.Fatalf("queued request reply_to %q != pending channel %q", req.ReplyTo, channel)
	}
	// Late reply lands on the leftover channel and is collectable with plain recv.
	if code, _, stderr := run("reply", "--room", room, "--channel", channel,
		"--from", "codex", "--body", "late answer"); code != 0 {
		t.Fatalf("late reply failed: %s", stderr)
	}
	code, stdout, _ = run("recv", "--room", room, "--as", channel, "--timeout", "5", "--format", "body")
	if code != 0 || strings.TrimSpace(stdout) != "late answer" {
		t.Fatalf("late collect: code=%d out=%q", code, stdout)
	}
}

func TestBodyFileReadErrorExitsOne(t *testing.T) {
	code, _, stderr := run("send", "--room", t.TempDir(), "--to", "x", "--from", "y",
		"--body-file", "/nonexistent-tincan-body")
	if code != 1 {
		t.Fatalf("want exit 1 for unreadable --body-file, got %d (stderr=%q)", code, stderr)
	}
}

// writePresenceFile writes a single token file under
// <room>/.tincan/present/<name>/, in the same on-disk JSON shape and layout
// spool.Recv's heartbeat produces (one file per parked Recv, named by a
// unique token), so CLI tests stay hermetic (no spawned processes, no
// parking a real Recv).
func writePresenceFile(t *testing.T, room, name string, pid int, since time.Time) {
	t.Helper()
	dir := filepath.Join(room, ".tincan", "present", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"since":%q}`, pid, since.UTC().Format(time.RFC3339))
	tok := filepath.Join(dir, fmt.Sprintf("tok-%d", time.Now().UnixNano()))
	if err := os.WriteFile(tok, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPingExitsZeroForLivePresence(t *testing.T) {
	room := t.TempDir()
	writePresenceFile(t, room, "codex", os.Getpid(), time.Now())
	code, stdout, stderr := run("ping", "--room", room, "--to", "codex")
	if code != ExitOK {
		t.Fatalf("ping exit %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
	want := fmt.Sprintf("present pid=%d", os.Getpid())
	if strings.TrimSpace(stdout) != want {
		t.Fatalf("ping stdout = %q, want %q", stdout, want)
	}
}

func TestPingExitsOneForAbsentPresence(t *testing.T) {
	room := t.TempDir()
	code, stdout, _ := run("ping", "--room", room, "--to", "nobody")
	if code != ExitError {
		t.Fatalf("ping exit %d, want %d", code, ExitError)
	}
	if strings.TrimSpace(stdout) != "absent" {
		t.Fatalf("ping stdout = %q, want %q", stdout, "absent")
	}
}

func TestPingExitsOneForDeadPIDPresence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("processAlive is conservative on Windows (see alive_other.go); dead-PID detection is unix-only")
	}
	room := t.TempDir()
	writePresenceFile(t, room, "ghost", 1<<30, time.Now())
	code, stdout, _ := run("ping", "--room", room, "--to", "ghost")
	if code != ExitError {
		t.Fatalf("ping exit %d, want %d", code, ExitError)
	}
	if strings.TrimSpace(stdout) != "absent" {
		t.Fatalf("ping stdout = %q, want %q", stdout, "absent")
	}
}

func TestPingRequiresTo(t *testing.T) {
	if code, _, _ := run("ping", "--room", t.TempDir()); code != ExitUsage {
		t.Fatalf("ping without --to: want exit %d, got %d", ExitUsage, code)
	}
}

func TestStatusListsQueuedNameWithoutPresence(t *testing.T) {
	room := t.TempDir()
	if code, _, stderr := run("send", "--room", room, "--to", "codex", "--from", "orch", "--body", "hi"); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	code, stdout, stderr := run("status", "--room", room)
	if code != ExitOK {
		t.Fatalf("status exit %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "codex") {
		t.Fatalf("status output missing name %q: %q", "codex", stdout)
	}
	if !strings.Contains(stdout, "1") {
		t.Fatalf("status output missing queued count: %q", stdout)
	}
}

func TestStatusEmptyRoomExitsOK(t *testing.T) {
	code, _, stderr := run("status", "--room", t.TempDir())
	if code != ExitOK {
		t.Fatalf("status on empty room exit %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
}

func TestStatusJSONFormatRoundTrips(t *testing.T) {
	room := t.TempDir()
	if code, _, stderr := run("send", "--room", room, "--to", "codex", "--from", "orch", "--body", "hi"); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	writePresenceFile(t, room, "codex", os.Getpid(), time.Now())
	code, stdout, stderr := run("status", "--room", room, "--format", "json")
	if code != ExitOK {
		t.Fatalf("status exit %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
	var got []spool.Presence
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("status --format json output does not parse: %v\n%s", err, stdout)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 presence entry, got %d: %+v", len(got), got)
	}
	if got[0].Name != "codex" || got[0].Queued != 1 || !got[0].Alive || got[0].PID != os.Getpid() {
		t.Fatalf("unexpected presence entry: %+v", got[0])
	}
}

func TestStatusUsageErrors(t *testing.T) {
	cases := [][]string{
		{"status", "--format", "yaml"},
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != ExitUsage {
			t.Fatalf("args %v: want exit %d, got %d", args, ExitUsage, code)
		}
	}
}

func TestReplyUsageErrors(t *testing.T) {
	cases := [][]string{
		{"reply"},                             // missing channel
		{"reply", "--channel", "r-x"},         // missing body
		{"ask", "--to", "x", "--from", "y"},   // missing body
		{"ask", "--from", "y", "--body", "b"}, // missing to
		{"ask", "--to", "x", "--from", "y", "--body", "b", "--format", "yaml"}, // bad format
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("args %v: want exit 2, got %d", args, code)
		}
	}
}

func TestStopQueuesStopKindEnvelope(t *testing.T) {
	room := t.TempDir()
	code, stdout, stderr := run("stop", "--room", room, "--to", "codex", "--from", "orch")
	if code != ExitOK {
		t.Fatalf("stop exit %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "to=codex") {
		t.Fatalf("stop stdout missing to=codex: %q", stdout)
	}
	code, recvOut, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "5")
	if code != ExitOK {
		t.Fatalf("recv exit %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
	e, err := envelope.Unmarshal([]byte(recvOut))
	if err != nil {
		t.Fatalf("recv output is not an envelope: %v\n%s", err, recvOut)
	}
	if e.Kind != "stop" {
		t.Fatalf("want Kind == \"stop\", got %q (envelope=%+v)", e.Kind, e)
	}
	if e.From != "orch" || e.To != "codex" {
		t.Fatalf("wrong envelope: %+v", e)
	}
}

func TestStopUsageErrors(t *testing.T) {
	cases := [][]string{
		{"stop", "--to", "x"},   // missing --from
		{"stop", "--from", "y"}, // missing --to
		{"stop"},                // missing both
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != ExitUsage {
			t.Fatalf("args %v: want exit %d, got %d", args, ExitUsage, code)
		}
	}
}

// canonTempDir returns a fresh t.TempDir() with symlinks/short-names resolved
// via filepath.EvalSymlinks (falling back to the raw path if that errors, e.g.
// on a platform where it isn't meaningful). The roomRootWarning tests build
// both the room path they pass in and the gitroot they expect back from this,
// so the string compare holds where the OS's temp dir is a symlink (macOS
// /var → /private/var) or an 8.3 short name (Windows RUNNER~1 vs runneradmin).
func canonTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

func TestRoomRootWarningNonRootChildOfGitDir(t *testing.T) {
	// Canonicalize the temp root so the room path we pass in and the gitroot we
	// expect back are on the same footing as what roomRootWarning reports.
	// roomRootWarning names paths as filepath.Abs(room) walks them, which does
	// NOT resolve symlinks; on macOS /var is a symlink to /private/var and on
	// Windows t.TempDir() can hand back an 8.3 short name (RUNNER~1), so a raw
	// t.TempDir() and its EvalSymlinks'd form differ and the substring check
	// fails. Building both the room and the expected gitroot from the resolved
	// root sidesteps that on every platform without touching production code.
	tmp := canonTempDir(t)
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(tmp, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	got := roomRootWarning(sub)
	if got == "" {
		t.Fatalf("want non-empty warning for %s (child of repo root %s), got empty", sub, tmp)
	}
	if !strings.Contains(got, tmp) {
		t.Fatalf("warning %q does not name gitroot %q", got, tmp)
	}
}

func TestRoomRootWarningAtGitDirRoot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if got := roomRootWarning(tmp); got != "" {
		t.Fatalf("want empty warning at repo root %s, got %q", tmp, got)
	}
}

// gitFreeTempDir returns a fresh temp directory none of whose ancestors
// (up to the filesystem root) contain a .git entry. t.TempDir() alone
// doesn't guarantee this: it roots under os.TempDir() (typically /tmp),
// and some machines have an unrelated stray .git sitting directly in /tmp
// or another ancestor shared by every temp dir on that host. Since
// roomRootWarning's contract is to walk all the way to the filesystem
// root, a test for "no .git anywhere" must control that entire chain
// itself rather than trust ambient host state.
func gitFreeTempDir(t *testing.T) string {
	t.Helper()
	isClean := func(dir string) bool {
		for {
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				return false
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return true
			}
			dir = parent
		}
	}
	if dir := t.TempDir(); isClean(dir) {
		return dir
	}
	// Ambient os.TempDir() is contaminated (e.g. a stray /tmp/.git on this
	// host) — fall back to a private root under the user's home directory,
	// well clear of both the shared temp tree and this repo checkout.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("gitFreeTempDir: no clean ancestor under os.TempDir() and UserHomeDir failed: %v", err)
	}
	fallbackRoot := filepath.Join(home, ".cache", "tincan-test-tmp")
	if err := os.MkdirAll(fallbackRoot, 0o755); err != nil {
		t.Fatalf("gitFreeTempDir: mkdir fallback root: %v", err)
	}
	if !isClean(fallbackRoot) {
		t.Fatalf("gitFreeTempDir: no git-free temp location found under os.TempDir() or %s", fallbackRoot)
	}
	dir, err := os.MkdirTemp(fallbackRoot, "roomroot")
	if err != nil {
		t.Fatalf("gitFreeTempDir: MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestRoomRootWarningNoGitAnywhere(t *testing.T) {
	base := gitFreeTempDir(t)
	if got := roomRootWarning(base); got != "" {
		t.Fatalf("want empty warning outside any repo, got %q", got)
	}
}

func TestRoomRootWarningGitAsFileIsRoot(t *testing.T) {
	// Git worktrees use a `.git` file (not a directory) pointing at the main
	// repo's git-dir. A room at such a worktree root must still count as a
	// root — no warning.
	tmp := t.TempDir()
	gitFile := filepath.Join(tmp, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /somewhere/else\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	if got := roomRootWarning(tmp); got != "" {
		t.Fatalf("want empty warning at worktree root %s (.git is a file), got %q", tmp, got)
	}
}

// setHomeEnv points os.UserHomeDir() at home for the duration of the test, on
// whichever platform is running. os.UserHomeDir reads $HOME on unix but
// %USERPROFILE% (falling back to %HOMEDRIVE%+%HOMEPATH%) on Windows, so the
// floor tests must set the variable the current OS actually consults or the
// floor never sees the temp home. Setting all of them is harmless.
func setHomeEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if vol := filepath.VolumeName(home); vol != "" {
		t.Setenv("HOMEDRIVE", vol)
		t.Setenv("HOMEPATH", home[len(vol):])
	}
}

func TestRoomRootWarningFlooredAtHome(t *testing.T) {
	// gitroot == $HOME (the common dotfiles-in-~ case): the warning's own
	// advice would be "pass --room ~", which is actively wrong, so it must
	// be suppressed. Canonicalize the temp home (see canonTempDir) and set the
	// per-OS home var (see setHomeEnv) so the floor's filepath.Rel(gitroot,
	// home) compares two paths in the same form on macOS/Windows too.
	tmpHome := canonTempDir(t)
	setHomeEnv(t, tmpHome)
	if err := os.Mkdir(filepath.Join(tmpHome, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(tmpHome, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if got := roomRootWarning(sub); got != "" {
		t.Fatalf("want empty warning for %s (gitroot is $HOME %s), got %q", sub, tmpHome, got)
	}
}

func TestRoomRootWarningNotFlooredBelowHome(t *testing.T) {
	// gitroot strictly below $HOME (e.g. ~/projects/foo) is a genuine
	// project repo — the floor must not suppress this real footgun.
	tmpHome := canonTempDir(t)
	setHomeEnv(t, tmpHome)
	gitroot := filepath.Join(tmpHome, "projects", "foo")
	if err := os.MkdirAll(filepath.Join(gitroot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(gitroot, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	got := roomRootWarning(sub)
	if got == "" {
		t.Fatalf("want non-empty warning for %s (gitroot %s is below $HOME), got empty", sub, gitroot)
	}
	if !strings.Contains(got, gitroot) {
		t.Fatalf("warning %q does not name gitroot %q", got, gitroot)
	}
}

func TestRoomRootWarningNotFlooredOutsideHome(t *testing.T) {
	// gitroot is not above $HOME at all (a separate tree, $HOME elsewhere)
	// — repos outside home still warn normally.
	tmpHome := canonTempDir(t)
	setHomeEnv(t, tmpHome)
	other := canonTempDir(t)
	if err := os.Mkdir(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(other, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	got := roomRootWarning(sub)
	if got == "" {
		t.Fatalf("want non-empty warning for %s (gitroot %s is outside $HOME %s), got empty", sub, other, tmpHome)
	}
	if !strings.Contains(got, other) {
		t.Fatalf("warning %q does not name gitroot %q", got, other)
	}
}
