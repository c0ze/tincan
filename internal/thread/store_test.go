// internal/thread/store_test.go
package thread

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func room(t *testing.T) string {
	t.Helper()
	r, _ := filepath.EvalSymlinks(t.TempDir())
	return r
}

func TestCreateOpenList(t *testing.T) {
	r := room(t)
	th, err := Create(r, "audit", "claude", "c1", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.ID) != 8 {
		t.Fatalf("id %q", th.ID)
	}
	if _, err := Open(r, th.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, "../../etc"); err == nil {
		t.Fatal("invalid id accepted")
	}
	list, err := List(r)
	if err != nil || len(list) != 1 || list[0].Title != "audit" || list[0].Status != StatusOpen || list[0].Budget != 6 {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestAppendAssignsSeqAndDisplayOrder(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	var ids []string
	err := th.Update(context.Background(), func(tx *Tx) error {
		ids = append(ids, tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "a"}).ID)
		ids = append(ids, tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "b"}).ID)
		tx.Append(Event{Kind: KindState, Message: ids[0], State: StateDone, Text: "a2"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := th.Snapshot()
	if snap.MaxSeq != 3 || len(snap.Messages) != 2 {
		t.Fatalf("snap %+v", snap)
	}
	if snap.Messages[0].ID != ids[0] || snap.Messages[0].Text != "a2" || snap.Messages[0].Seq != 3 || snap.Messages[1].N != 2 {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestConcurrentUpdatesKeepSeqStrict(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				th2, _ := Open(th.Room, th.ID) // separate handle, as another process would have
				th2.Update(context.Background(), func(tx *Tx) error {
					tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "x"})
					return nil
				})
			}
		}()
	}
	wg.Wait()
	evs, err := th.events()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 80 {
		t.Fatalf("%d events", len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

func TestTornTailIsIgnoredThenRepaired(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "whole"})
		return nil
	})
	f, _ := os.OpenFile(th.eventsPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`[{"seq":2,"kind":"mess`)
	f.Close()
	snap, err := th.Snapshot()
	if err != nil || len(snap.Messages) != 1 {
		t.Fatalf("torn tail not ignored: %+v %v", snap, err)
	}
	if err := th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "next"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap, err = th.Snapshot()
	if err != nil || len(snap.Messages) != 2 || snap.Messages[1].Seq != 2 {
		t.Fatalf("after repair: %+v %v", snap.Messages, err)
	}
}

func TestTransactionIsAtomicUnderTornWrite(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "kept"})
		return nil
	})
	before, _ := os.Stat(th.eventsPath())
	th.Update(context.Background(), func(tx *Tx) error {
		id := tx.Append(Event{Kind: KindMessage, Author: "a.t", Role: RoleAgent}).ID
		tx.Append(Event{Kind: KindIntent, Message: id, Listener: "a.t", RequestID: "x-" + id})
		tx.Append(Event{Kind: KindState, Message: id, State: StatePending})
		return nil
	})
	after, _ := os.Stat(th.eventsPath())
	// Simulate a crash that persisted only part of the second transaction.
	os.Truncate(th.eventsPath(), before.Size()+(after.Size()-before.Size())/2)
	snap, err := th.Snapshot()
	if err != nil || len(snap.Messages) != 1 || snap.Messages[0].Text != "kept" {
		t.Fatalf("partial transaction visible: %+v %v", snap.Messages, err)
	}
}

func TestClientIDLookupAndMetaSave(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "x", ClientID: "k1"})
		tx.Meta.Status = StatusStopping
		tx.SaveMeta()
		return nil
	})
	snap, _ := th.Snapshot()
	if _, ok := snap.ByClientID("k1"); !ok || snap.Meta.Status != StatusStopping {
		t.Fatalf("snap %+v", snap)
	}
}

func TestSaveMetaCapturesFinalMetaAtCommit(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	err := th.Update(context.Background(), func(tx *Tx) error {
		tx.SaveMeta()
		// Changes made after SaveMeta must still be in the committed event.
		if tx.Meta.Listeners == nil {
			tx.Meta.Listeners = map[string]string{}
		}
		tx.Meta.Listeners["claude.abcd1234"] = "claude"
		tx.Meta.Status = StatusStopping
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := th.events()
	if err != nil {
		t.Fatal(err)
	}
	var last *Event
	for i := range evs {
		if evs[i].Kind == KindThread {
			last = &evs[i]
		}
	}
	if last == nil || last.Meta == nil {
		t.Fatalf("no thread event carrying meta: %+v", evs)
	}
	if last.Meta.Status != StatusStopping || last.Meta.Listeners["claude.abcd1234"] != "claude" {
		t.Fatalf("committed event does not carry final meta: %+v", last.Meta)
	}
	snap, _ := th.Snapshot()
	if snap.Meta.Status != StatusStopping || snap.Meta.Listeners["claude.abcd1234"] != "claude" {
		t.Fatalf("snapshot meta: %+v", snap.Meta)
	}
}

func TestJournalMetaSurvivesMetaWriteCrash(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	before, err := os.ReadFile(th.metaPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := th.Update(context.Background(), func(tx *Tx) error {
		tx.Meta.Status = StatusStopping
		tx.SaveMeta()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between the event commit and the thread.json write: put
	// the OLD cached bytes back, as if writeMeta never ran.
	if err := os.WriteFile(th.metaPath(), before, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := th.Snapshot()
	if err != nil || snap.Meta.Status != StatusStopping {
		t.Fatalf("journal-derived meta lost after simulated crash: %+v %v", snap.Meta, err)
	}
	// The next Update must repair the stale cache to match the journal.
	if err := th.Update(context.Background(), func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(th.metaPath())
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	if err := json.Unmarshal(repaired, &m); err != nil || m.Status != StatusStopping {
		t.Fatalf("thread.json not repaired: %s", repaired)
	}
}

func TestUppercaseThreadIDRejected(t *testing.T) {
	r := room(t)
	th, err := Create(r, "t", "claude", "", 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, strings.ToUpper(th.ID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uppercase id accepted: %v", err)
	}
}

func TestPromptFilesRoundTrip(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	if err := th.WritePrompt("abc-m1", "hello ü"); err != nil {
		t.Fatal(err)
	}
	got, err := th.ReadPrompt("abc-m1")
	if err != nil || got != "hello ü" {
		t.Fatalf("%q %v", got, err)
	}
}
