package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadPrefixSeparatesAllocationAndFileSizeBounds(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "large-record")
	data := "header" + strings.Repeat("x", 256<<10)
	if err := WriteFileAtomic(path, []byte(data)); err != nil {
		t.Fatal(err)
	}
	prefix, err := ReadPrefix(path, 6, int64(len(data)))
	if err != nil || string(prefix) != "header" {
		t.Fatalf("bounded prefix from large file: %q, %v", prefix, err)
	}
	if _, err := ReadPrefix(path, 6, int64(len(data)-1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("total file bound was lost: %v", err)
	}
	for _, limits := range [][2]int64{{-1, 100}, {10, 9}, {0, -1}, {0, 1<<63 - 1}} {
		if _, err := ReadPrefix(path, limits[0], limits[1]); err == nil {
			t.Fatalf("invalid limits accepted: %v", limits)
		}
	}
}

func TestReadPrefixRejectsSymlinksAndDirectories(t *testing.T) {
	dir := canonicalTemp(t)
	if _, err := ReadPrefix(dir, 16, 1024); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("directory accepted as snapshot: %v", err)
	}
	path := filepath.Join(dir, "file")
	if err := WriteFileAtomic(path, []byte("data")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := ReadPrefix(link, 16, 1024); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("symlink accepted as snapshot: %v", err)
	}
}

func TestReadPrefixToleratesAtomicReplacement(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "state")
	old, fresh := strings.Repeat("a", 128), strings.Repeat("b", 128)
	if err := WriteFileAtomic(path, []byte(old+strings.Repeat("x", 64<<10))); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 48; i++ {
			data := old
			if i%2 == 0 {
				data = fresh
			}
			if err := WriteFileAtomic(path, []byte(data+strings.Repeat("x", 64<<10))); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		data, err := ReadPrefix(path, 128, 128<<10)
		if err != nil {
			<-done
			t.Fatalf("healthy replacement rejected: %v", err)
		}
		if string(data) != old && string(data) != fresh {
			<-done
			t.Fatalf("torn prefix: %q", data)
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
