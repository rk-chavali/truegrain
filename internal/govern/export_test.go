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
