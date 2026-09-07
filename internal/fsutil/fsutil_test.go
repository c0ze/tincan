package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func canonicalTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadFileSizeBound(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "data")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path, 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized read: %v", err)
	}
	data, err := ReadFile(path, 5)
	if err != nil || string(data) != "12345" {
		t.Fatalf("bounded read=%q, %v", data, err)
	}
	for _, limit := range []int64{-1, 1<<63 - 1} {
		if _, err := ReadFile(path, limit); err == nil {
			t.Errorf("invalid limit %d accepted", limit)
		}
	}
}

func TestAtomicReplacementMakesFilePrivateAndCleansTemporaryFiles(t *testing.T) {
	dir := canonicalTemp(t)
	path := filepath.Join(dir, "data")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(path, 3)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement=%q, %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary residue: %v, %v", entries, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions=%o", info.Mode().Perm())
		}
	}
}

func TestReadAndCreateRejectSymlinks(t *testing.T) {
	dir := canonicalTemp(t)
	outside := canonicalTemp(t)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := MkdirPrivate(filepath.Join(link, "child")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("Mkdir followed symlink: %v", err)
	}
	if err := WriteFileAtomic(filepath.Join(link, "data"), []byte("secret")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("Write followed symlink: %v", err)
	}
	if _, err := ReadFile(link, 10); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("Read followed symlink: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("modified external directory: %v, %v", entries, err)
	}
}

func TestOpenFileAppendAndRejectSymlinkBeforeTruncating(t *testing.T) {
	dir := canonicalTemp(t)
	path := filepath.Join(dir, "output")
	f, err := OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("first"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	f, err = OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	data, err := ReadFile(path, 100)
	if err != nil || string(data) != "firstsecond" {
		t.Fatalf("append=%q, %v", data, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if f, err := OpenFile(link, os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		f.Close()
		t.Fatal("followed symlink during truncation")
	}
	data, err = ReadFile(path, 100)
	if err != nil || string(data) != "firstsecond" {
		t.Fatalf("symlink target modified=%q, %v", data, err)
	}
}

func TestReadFileAcceptsCompleteSnapshotsDuringAtomicReplacement(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "state")
	old, fresh := strings.Repeat("a", 128), strings.Repeat("b", 128)
	if err := WriteFileAtomic(path, []byte(old)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 24; i++ {
			body := old
			if i%2 == 0 {
				body = fresh
			}
			if err := WriteFileAtomic(path, []byte(body)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		data, err := ReadFile(path, 128)
		if err != nil {
			<-done
			t.Fatalf("healthy atomic replacement rejected: %v", err)
		}
		if string(data) != old && string(data) != fresh {
			<-done
			t.Fatalf("torn snapshot: %q", data)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}
