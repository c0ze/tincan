package thread

import "time"

// SetRedriveEvery spaces host relaunches of running turns (tests use a
// short interval).
func SetRedriveEvery(d *Dispatcher, every time.Duration) { d.redriveEvery = every }

// SetRedriveWait bounds one probe, relaunch or settlement.
func SetRedriveWait(d *Dispatcher, wait time.Duration) { d.redriveWait = wait }

// SetRedrive also sets how many failed relaunches end a turn.
func SetRedrive(d *Dispatcher, every time.Duration, max int) {
	d.redriveEvery, d.redriveMax = every, max
}
