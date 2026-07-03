package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// writePresenceFile writes <room>/.tincan/present/<name> directly, in the
// same on-disk JSON shape spool.Recv's heartbeat produces, so CLI tests stay
// hermetic (no spawned processes, no parking a real Recv).
func writePresenceFile(t *testing.T, room, name string, pid int, since time.Time) {
	t.Helper()
	dir := filepath.Join(room, ".tincan", "present")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"since":%q}`, pid, since.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
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

func TestRoomRootWarningNonRootChildOfGitDir(t *testing.T) {
	tmp := t.TempDir()
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
	resolvedTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", tmp, err)
	}
	if !strings.Contains(got, resolvedTmp) {
		t.Fatalf("warning %q does not name gitroot %q", got, resolvedTmp)
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

func TestRoomRootWarningFlooredAtHome(t *testing.T) {
	// gitroot == $HOME (the common dotfiles-in-~ case): the warning's own
	// advice would be "pass --room ~", which is actively wrong, so it must
	// be suppressed.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
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
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
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
	resolvedGitroot, err := filepath.EvalSymlinks(gitroot)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", gitroot, err)
	}
	if !strings.Contains(got, resolvedGitroot) {
		t.Fatalf("warning %q does not name gitroot %q", got, resolvedGitroot)
	}
}

func TestRoomRootWarningNotFlooredOutsideHome(t *testing.T) {
	// gitroot is not above $HOME at all (a separate tree, $HOME elsewhere)
	// — repos outside home still warn normally.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	other := t.TempDir()
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
	resolvedOther, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", other, err)
	}
	if !strings.Contains(got, resolvedOther) {
		t.Fatalf("warning %q does not name gitroot %q", got, resolvedOther)
	}
}
