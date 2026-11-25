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
	for {
		_, err = unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ESRCH {
			return nil // already exited, but still unreaped
		}
		if err != nil {
			return err
		}
		break
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(fd, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			evErr := unix.Errno(events[0].Data)
			if evErr == unix.ESRCH {
				return nil
			}
			return evErr
		}
		return nil
	}
}
