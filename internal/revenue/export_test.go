package revenue

import "time"

// UntilNextRun exposes the schedule calculation to the external revenue_test
// package. The rule is "02:00 IST, tomorrow if today's has passed", and its
// edges — the minute before, the minute after, and a caller in a different
// timezone — are exactly where a nightly financial job silently runs at the
// wrong hour.
//
// Compiled only under `go test`, so this does not widen the package API.
func UntilNextRun(s *ReconcileScheduler, from time.Time) time.Duration {
	return s.untilNextRun(from)
}

// SchedulerZone reports the location the scheduler resolved, so a test can
// assert it found Asia/Kolkata rather than having quietly fallen back to UTC.
func SchedulerZone(s *ReconcileScheduler) string {
	return s.loc.String()
}
