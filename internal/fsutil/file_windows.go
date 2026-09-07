//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const openRejectsSymlinks = true

// Share deletion so an atomic state replacement can succeed while a reader
// still holds the previous version. OPEN_REPARSE_POINT and handle attributes
// prevent following a symlink swapped in between Lstat and CreateFile.
func openFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(abs, `\\?\`) {
		if strings.HasPrefix(abs, `\\`) {
			abs = `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
		} else {
			abs = `\\?\` + abs
		}
	}
	ptr, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, err
	}
	var access uint32
	switch flags & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		return nil, os.ErrInvalid
	}
	if flags&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	if flags&os.O_APPEND != 0 {
		access &^= windows.GENERIC_WRITE
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE
	}
	access |= windows.FILE_READ_ATTRIBUTES
	creation := uint32(windows.OPEN_EXISTING)
	if flags&os.O_CREATE != 0 {
		creation = windows.OPEN_ALWAYS
		if flags&os.O_EXCL != 0 {
			creation = windows.CREATE_NEW
		}
	}
	attrs := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if flags&os.O_CREATE != 0 && mode.Perm()&0o200 == 0 {
		attrs |= windows.FILE_ATTRIBUTE_READONLY
	} else {
		attrs |= windows.FILE_ATTRIBUTE_NORMAL
	}
	if flags&os.O_SYNC != 0 {
		attrs |= windows.FILE_FLAG_WRITE_THROUGH
	}
	h, err := windows.CreateFile(ptr, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, creation, attrs, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(h, &info)
	kind, kindErr := windows.GetFileType(h)
	if infoErr != nil || kindErr != nil || kind != windows.FILE_TYPE_DISK || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrUnsafeFile}
	}
	return os.NewFile(uintptr(h), path), nil
}

// Directory syncing is not exposed portably on Windows. File contents are
// flushed before replacement; power-loss durability remains OS-specific.
func SyncDir(string) error { return nil }

func renameFile(oldPath, newPath string) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = os.Rename(oldPath, newPath)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if attempt < 7 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return err
}
