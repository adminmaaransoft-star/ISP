package revenue_test

import (
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// A nightly financial job that runs at the wrong hour is worse than one that
// does not run at all: the figures look present but describe a partial day.
// These pin the schedule arithmetic, including the timezone the whole rule
// depends on.

func mustIST(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	return loc
}

func TestFR_REV_001_Scheduler_ResolvesISTNotUTC(t *testing.T) {
	mustIST(t) // skip rather than fail where tzdata is genuinely absent

	s := revenue.NewReconcileScheduler(nil)
	if got := revenue.SchedulerZone(s); got != "Asia/Kolkata" {
		t.Errorf("scheduler zone: want Asia/Kolkata, got %q — 02:00 would fall in the working day", got)
	}
}

func TestFR_REV_001_Scheduler_NextRunIsAlwaysTheNext0200IST(t *testing.T) {
	ist := mustIST(t)
	s := revenue.NewReconcileScheduler(nil)

	cases := []struct {
		name     string
		from     time.Time
		wantWait time.Duration
	}{
		{
			name:     "just before the run, waits minutes",
			from:     time.Date(2026, 5, 10, 1, 30, 0, 0, ist),
			wantWait: 30 * time.Minute,
		},
		{
			name:     "just after the run, waits almost a full day",
			from:     time.Date(2026, 5, 10, 2, 1, 0, 0, ist),
			wantWait: 23*time.Hour + 59*time.Minute,
		},
		{
			name:     "exactly at 02:00 waits for tomorrow, never fires twice",
			from:     time.Date(2026, 5, 10, 2, 0, 0, 0, ist),
			wantWait: 24 * time.Hour,
		},
		{
			name:     "midday waits until the small hours",
			from:     time.Date(2026, 5, 10, 12, 0, 0, 0, ist),
			wantWait: 14 * time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := revenue.UntilNextRun(s, tc.from); got != tc.wantWait {
				t.Errorf("want %s, got %s", tc.wantWait, got)
			}
		})
	}
}

// TestFR_REV_001_Scheduler_CallerTimezoneDoesNotShiftTheRun — the schedule is
// defined in IST regardless of where the process thinks it is. A server in UTC
// must still reconcile at 02:00 IST, not 02:00 local.
func TestFR_REV_001_Scheduler_CallerTimezoneDoesNotShiftTheRun(t *testing.T) {
	ist := mustIST(t)
	s := revenue.NewReconcileScheduler(nil)

	istNoon := time.Date(2026, 5, 10, 12, 0, 0, 0, ist)
	fromIST := revenue.UntilNextRun(s, istNoon)
	fromUTC := revenue.UntilNextRun(s, istNoon.UTC())

	if fromIST != fromUTC {
		t.Errorf("the same instant must schedule identically regardless of the caller's zone: IST=%s UTC=%s", fromIST, fromUTC)
	}
}
