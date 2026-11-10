package spool

import (
	"time"

	"golang.org/x/sys/windows"
)

// stampMoved sets a just-moved log or quarantine entry's mtime to now. Rename
// keeps the original mtime, and GC ages these entries by mtime, so without it
// a message that waited longer than the retention would be collected by the
// very next gc. FILE_FLAG_OPEN_REPARSE_POINT stamps a quarantined symlink or
// junction itself, never its target. Best-effort: the move has already
// committed.
func stampMoved(path string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	h, err := windows.CreateFile(p, windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	now := windows.NsecToFiletime(time.Now().UnixNano())
	windows.SetFileTime(h, nil, &now, &now)
}
