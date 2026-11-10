package thread

import "time"

// SetRedriveEvery spaces host relaunches of running turns (tests use a
// short interval).
func SetRedriveEvery(d *Dispatcher, every time.Duration) { d.redriveEvery = every }

// SetRedrive also sets how many failed relaunches end a turn.
func SetRedrive(d *Dispatcher, every time.Duration, max int) {
	d.redriveEvery, d.redriveMax = every, max
}
