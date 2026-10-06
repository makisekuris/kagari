package digest

import (
	"testing"
	"time"

	"kagari/internal/config"
)

func TestWeeklyDigestCheck(t *testing.T) {
	utc := time.UTC
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start, end := PreviousWeek(time.Date(2025, 1, 1, 12, 0, 0, 0, utc), utc)
	if !start.Equal(time.Date(2024, 12, 23, 0, 0, 0, 0, utc)) || !end.Equal(time.Date(2024, 12, 30, 0, 0, 0, 0, utc)) {
		t.Fatalf("year boundary PreviousWeek() = [%s,%s)", start, end)
	}
	start, end = PreviousWeek(time.Date(2024, 3, 11, 12, 0, 0, 0, newYork), newYork)
	if !start.Equal(time.Date(2024, 3, 4, 0, 0, 0, 0, newYork)) || !end.Equal(time.Date(2024, 3, 11, 0, 0, 0, 0, newYork)) || end.Sub(start) != 167*time.Hour {
		t.Fatalf("DST PreviousWeek() = [%s,%s), duration %s", start, end, end.Sub(start))
	}
	sundayStart, sundayEnd := PreviousWeek(time.Date(2024, 3, 10, 9, 0, 0, 0, newYork), newYork)
	if !sundayStart.Equal(time.Date(2024, 2, 26, 0, 0, 0, 0, newYork)) || !sundayEnd.Equal(time.Date(2024, 3, 4, 0, 0, 0, 0, newYork)) {
		t.Fatalf("Sunday PreviousWeek() = [%s,%s)", sundayStart, sundayEnd)
	}

	weekly := config.Weekly{Enabled: true, Timezone: "America/New_York", Weekday: 1, Time: "09:00"}
	enabledAt := time.Date(2024, 3, 4, 8, 0, 0, 0, newYork)
	now := time.Date(2024, 3, 18, 9, 0, 0, 0, newYork)
	due, err := Due(now, enabledAt, weekly)
	if err != nil || len(due) != 3 {
		t.Fatalf("Due() = (%+v, %v), want three missed weekly periods", due, err)
	}
	seen := map[string]bool{}
	for _, request := range due {
		key := request.Start.String() + "/" + request.End.String()
		if seen[key] {
			t.Fatalf("Due() repeated period %s", key)
		}
		seen[key] = true
	}
	if !due[1].Start.Equal(time.Date(2024, 3, 4, 9, 0, 0, 0, newYork)) || !due[1].End.Equal(time.Date(2024, 3, 11, 9, 0, 0, 0, newYork)) {
		t.Fatalf("DST due period = [%s,%s)", due[1].Start, due[1].End)
	}
	if !due[0].End.Equal(time.Date(2024, 3, 4, 9, 0, 0, 0, newYork)) || !due[2].End.Equal(time.Date(2024, 3, 18, 9, 0, 0, 0, newYork)) {
		t.Fatalf("catch-up windows should use each trigger as cutoff: %+v", due)
	}
	weekly.Weekday = 1
	exactEnabledAt := time.Date(2024, 3, 11, 9, 0, 0, 0, newYork)
	due, err = Due(time.Date(2024, 3, 18, 9, 0, 0, 0, newYork), exactEnabledAt, weekly)
	if err != nil || len(due) != 1 || !due[0].Start.Equal(time.Date(2024, 3, 11, 9, 0, 0, 0, newYork)) {
		t.Fatalf("Due() at exact enabled boundary = (%+v, %v)", due, err)
	}
	sundayGap := config.Weekly{Enabled: true, Timezone: "America/New_York", Weekday: 0, Time: "02:30"}
	gapEnabledAt := time.Date(2024, 3, 3, 3, 0, 0, 0, newYork)
	due, err = Due(time.Date(2024, 3, 10, 1, 59, 0, 0, newYork), gapEnabledAt, sundayGap)
	if err != nil || len(due) != 0 {
		t.Fatalf("Due() ran before nonexistent local time became valid: (%+v, %v)", due, err)
	}
	due, err = Due(time.Date(2024, 3, 10, 3, 0, 0, 0, newYork), gapEnabledAt, sundayGap)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due() missed first valid time after DST gap: (%+v, %v)", due, err)
	}

}
