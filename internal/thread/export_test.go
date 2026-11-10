package thread

import "time"

// SetRedriveEvery spaces host relaunches of running turns (tests use a
// short interval).
func SetRedriveEvery(d *Dispatcher, every time.Duration) { d.redriveEvery = every }
