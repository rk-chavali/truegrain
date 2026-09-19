package govern

import "time"

// SetReloadIntervalForTest shortens the throttle that keeps a busy server
// from statting its revocation and credential files once per request.
//
// The throttle is a performance decision and nothing about correctness
// depends on its value, so a test that waited it out would be spending
// seconds to prove nothing. Restoring it is the caller's job, which is what
// the returned function is for.
func SetReloadIntervalForTest(d time.Duration) (restore func()) {
	previous := reloadInterval
	reloadInterval = d
	return func() { reloadInterval = previous }
}

// ReloadNowForTest re-reads the file immediately, bypassing the throttle,
// and returns what the loader made of it.
//
// A test about rotation is about which tokens are accepted, not about how
// many seconds the throttle takes to fire. Polling for the change reported
// "the loader never picked it up" whether the machine was merely busy or
// the file had failed to parse, which is two very different bugs behind one
// message. The throttle firing on its own is worth proving once, and does
// not need proving again in every test that changes a file.
func ReloadNowForTest(c *Credentials) error { return c.reload() }
