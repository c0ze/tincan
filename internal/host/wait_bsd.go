//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package host

import "golang.org/x/sys/unix"

func waitChildExit(pid int) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	events := make([]unix.Kevent_t, 1)
	for {
		_, err = unix.Kevent(fd, []unix.Kevent_t{change}, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ESRCH {
			return nil
		} // already exited, but still unreaped
		return err
	}
}
