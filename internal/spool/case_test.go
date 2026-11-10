package spool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

// foldsCase reports whether the spool's filesystem resolves "Codex" and
// "codex" to the same inbox directory (macOS and Windows defaults).
func foldsCase(t *testing.T, sp *Spool) bool {
	t.Helper()
	if err := os.MkdirAll(sp.InboxDir("CaseProbe"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sp.InboxDir("CaseProbe"))
	upper, err := os.Lstat(sp.InboxDir("CaseProbe"))
	if err != nil {
		t.Fatal(err)
	}
	lower, err := os.Lstat(sp.InboxDir("caseprobe"))
	return err == nil && os.SameFile(upper, lower)
}

// On a case-folding filesystem a second spelling would share the first one's
// inbox and every message for it would be quarantined as misrouted. Send and
// claim refuse it, naming the spelling that owns the inbox.
func TestCaseAliasedNameIsRefused(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if !foldsCase(t, sp) {
		t.Skip("case-sensitive filesystem: names differing by case are separate inboxes")
	}
	first := msg("orch", "Codex", "for Codex", 1)
	if err := sp.Send(first); err != nil {
		t.Fatal(err)
	}
	if err := sp.Send(msg("orch", "codex", "for codex", 2)); err == nil || !strings.Contains(err.Error(), `"Codex"`) {
		t.Fatalf("aliased send error = %v, want one naming \"Codex\"", err)
	}
	if _, err := sp.ClaimContext(context.Background(), "codex", time.Second); err == nil || !strings.Contains(err.Error(), `"Codex"`) {
		t.Fatalf("aliased claim error = %v, want one naming \"Codex\"", err)
	}
	if _, _, err := sp.TryClaim(context.Background(), "codex"); err == nil {
		t.Fatal("aliased TryClaim succeeded")
	}
	got, err := sp.Recv("Codex", time.Second, false)
	if err != nil || got.ID != first.ID {
		t.Fatalf("existing spelling lost its message: %+v, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(sp.root, "quarantine")); !os.IsNotExist(err) {
		t.Fatalf("message quarantined: %v", err)
	}
}

// A message an older binary published under another spelling of the same
// (case-folded) inbox is delivered, never quarantined.
func TestCaseOnlyRoutingMismatchIsDelivered(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if !foldsCase(t, sp) {
		t.Skip("case-sensitive filesystem: a case-only mismatch is a real misroute")
	}
	if err := sp.Send(msg("orch", "codex", "first", 1)); err != nil {
		t.Fatal(err)
	}
	legacy := msg("orch", "Codex", "legacy spelling", 2)
	data, err := envelope.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp.InboxDir("codex"), envelope.Filename(legacy)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "legacy spelling"} {
		got, err := sp.Recv("codex", time.Second, false)
		if err != nil || got.Body != want {
			t.Fatalf("got %+v, %v; want %q", got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(sp.root, "quarantine")); !os.IsNotExist(err) {
		t.Fatalf("case-only mismatch quarantined: %v", err)
	}
}

// Where the filesystem keeps case, mixed-case names stay separate inboxes.
func TestMixedCaseNamesStaySeparateOnCaseSensitiveFS(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if foldsCase(t, sp) {
		t.Skip("case-folding filesystem")
	}
	for i, name := range []string{"Codex", "codex"} {
		if err := sp.Send(msg("orch", name, "for "+name, int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"codex", "Codex"} {
		got, err := sp.Recv(name, time.Second, false)
		if err != nil || got.Body != "for "+name {
			t.Fatalf("%s got %+v, %v", name, got, err)
		}
	}
	misrouted := msg("orch", "Codex", "wrong inbox", 3)
	data, _ := envelope.Marshal(misrouted)
	if err := os.WriteFile(filepath.Join(sp.InboxDir("codex"), envelope.Filename(misrouted)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Recv("codex", 100*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("misrouted message delivered: %v", err)
	}
}
