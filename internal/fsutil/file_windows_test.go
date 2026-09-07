//go:build windows

package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsAtomicReplacementWhileSafeReaderHoldsOldVersion(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "state")
	if err := WriteFileAtomic(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatalf("reader prevented replacement: %v", err)
	}
	old, err := io.ReadAll(f)
	if err != nil || string(old) != "old" {
		t.Fatalf("old snapshot=%q, %v", old, err)
	}
	fresh, err := ReadFile(path, 3)
	if err != nil || string(fresh) != "new" {
		t.Fatalf("new snapshot=%q, %v", fresh, err)
	}
}

func TestWindowsAtomicReplacementRetriesBriefExternalSharingViolation(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "state")
	if err := WriteFileAtomic(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); windows.CloseHandle(h); close(done) }()
	err = WriteFileAtomic(path, []byte("new"))
	<-done
	if err != nil {
		t.Fatalf("brief external sharing violation was not retried: %v", err)
	}
	data, err := ReadFile(path, 3)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement=%q, %v", data, err)
	}
}

func TestWindowsLowLevelOpenRejectsReparsePoint(t *testing.T) {
	dir := canonicalTemp(t)
	path, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
	if err := WriteFileAtomic(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	f, err := openFile(link, os.O_RDONLY, 0)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("reparse point open=%v", err)
	}
}
