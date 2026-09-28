package committee

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorePutListDeleteVersions(t *testing.T) {
	s := NewStore(stateDir(t))
	ctx := context.Background()
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty store: %v %v", list, err)
	}
	c, err := s.Put(ctx, Committee{Name: "reviewers", Members: []string{"codex@cachyos"}})
	if err != nil || c.Version != 1 || c.DeadlineMinutes != DefaultDeadlineMinutes {
		t.Fatalf("first put: %+v %v", c, err)
	}
	c, err = s.Put(ctx, Committee{Name: "reviewers", Version: 99, Members: []string{"codex@cachyos", "grok@macmini"}})
	if err != nil || c.Version != 2 {
		t.Fatalf("second put must ignore the client's version: %+v %v", c, err)
	}
	s.Put(ctx, Committee{Name: "alpha", Members: []string{"kimi@cachyos"}})
	list, _ := s.List()
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "reviewers" {
		t.Fatalf("list not sorted: %+v", list)
	}
	if ok, err := s.Delete(ctx, "reviewers"); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.Delete(ctx, "reviewers"); ok {
		t.Fatal("deleted twice")
	}
	c, _ = s.Put(ctx, Committee{Name: "reviewers", Members: []string{"codex@cachyos"}})
	if c.Version != 1 {
		t.Fatalf("recreated committee version = %d", c.Version)
	}
}

func TestStoreConcurrentPutsKeepEveryVersion(t *testing.T) {
	s := NewStore(stateDir(t))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(context.Background(), Committee{Name: "reviewers", Members: []string{"codex@cachyos"}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Version != 8 {
		t.Fatalf("lost update: %+v %v", list, err)
	}
}

func TestStoreMalformedFileIsAnErrorNotEmpty(t *testing.T) {
	dir := stateDir(t)
	s := NewStore(dir)
	os.WriteFile(s.Path(), []byte("{not json"), 0o600)
	if _, err := s.List(); err == nil || !strings.Contains(err.Error(), s.Path()) {
		t.Fatalf("malformed file: %v", err)
	}
	if _, err := s.Put(context.Background(), Committee{Name: "a", Members: []string{"x@m"}}); err == nil {
		t.Fatal("put overwrote a malformed file")
	}
	if data, _ := os.ReadFile(s.Path()); string(data) != "{not json" {
		t.Fatalf("file changed: %q", data)
	}
}

func TestStoreRejectsInvalidAndTooMany(t *testing.T) {
	s := NewStore(stateDir(t))
	if _, err := s.Put(context.Background(), Committee{Name: "you", Members: []string{"x@m"}}); err == nil {
		t.Fatal("stored an invalid committee")
	}
	for i := 0; i < MaxCommittees; i++ {
		if _, err := s.Put(context.Background(), Committee{Name: "c" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Members: []string{"x@m"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put(context.Background(), Committee{Name: "overflow", Members: []string{"x@m"}}); err == nil {
		t.Fatal("stored more than MaxCommittees")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := stateDir(t)
	if _, ok, err := LoadCache(dir); ok || err != nil {
		t.Fatalf("empty cache: %v %v", ok, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	want := Cache{From: "macmini", FetchedAt: now, AttemptedAt: now, Committees: []Committee{{Name: "a", Version: 3, Members: []string{"x@m"}, DeadlineMinutes: 30}}}
	if err := SaveCache(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadCache(dir)
	if err != nil || !ok || got.From != "macmini" || !got.FetchedAt.Equal(now) || len(got.Committees) != 1 || got.Committees[0].Version != 3 {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
}

// stateDir is a canonical temporary directory: fsutil refuses symlinked
// ancestors such as macOS's /var -> /private/var.
func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
