package personal

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func TestParseScheduleRequiresFiveFieldsAndSeparateTimezone(t *testing.T) {
	schedule, err := ParseSchedule("0 9 * * 1-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := schedule.(*cron.SpecSchedule).Location.String(); got != DefaultScheduleTimezone {
		t.Fatalf("default timezone = %q, want %q", got, DefaultScheduleTimezone)
	}

	for _, expression := range []string{
		"@daily",
		"0 0 0 * * *",
		"TZ=UTC 0 0 * * *",
		"CRON_TZ=UTC 0 0 * * *",
		"0 0 * * * echo run",
	} {
		t.Run(expression, func(t *testing.T) {
			if _, err := ParseSchedule(expression, "UTC"); err == nil {
				t.Fatalf("ParseSchedule(%q) succeeded", expression)
			}
		})
	}
	if _, err := ParseSchedule("0 0 * * *", "Mars/Olympus"); err == nil {
		t.Fatal("ParseSchedule accepted an invalid timezone")
	}
	if _, err := ParseSchedule("0 0 * * *", "Local"); err == nil {
		t.Fatal("ParseSchedule accepted the machine-local timezone")
	}
}

func TestNextScheduleTimeUsesIndependentTimezone(t *testing.T) {
	tests := []struct {
		name     string
		timezone string
		after    time.Time
		want     time.Time
	}{
		{
			name:  "default Shanghai",
			after: time.Date(2026, time.November, 6, 0, 59, 0, 0, time.UTC),
			want:  time.Date(2026, time.November, 6, 1, 0, 0, 0, time.UTC),
		},
		{
			name:     "explicit New York",
			timezone: "America/New_York",
			after:    time.Date(2026, time.November, 6, 0, 0, 0, 0, time.UTC),
			want:     time.Date(2026, time.November, 6, 14, 0, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NextScheduleTime("0 9 * * *", test.timezone, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("NextScheduleTime() = %s, want %s", got, test.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("NextScheduleTime() location = %s, want UTC", got.Location())
			}
		})
	}
}

func TestScheduleTimeAcrossCalendarBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		after      time.Time
		want       time.Time
	}{
		{
			name:       "next day",
			expression: "0 0 * * *",
			after:      time.Date(2026, time.November, 6, 23, 59, 0, 0, time.UTC),
			want:       time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "next month",
			expression: "0 0 1 * *",
			after:      time.Date(2026, time.January, 31, 0, 1, 0, 0, time.UTC),
			want:       time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:       "leap day",
			expression: "0 0 29 2 *",
			after:      time.Date(2024, time.February, 28, 12, 0, 0, 0, time.UTC),
			want:       time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NextScheduleTime(test.expression, "UTC", test.after)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("NextScheduleTime() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestScheduleTimeDST(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		after      time.Time
		want       time.Time
	}{
		{
			name:       "skip nonexistent spring time",
			expression: "30 2 * * *",
			after:      time.Date(2024, time.March, 10, 5, 0, 0, 0, time.UTC),
			want:       time.Date(2024, time.March, 11, 6, 30, 0, 0, time.UTC),
		},
		{
			name:       "repeat fall time",
			expression: "30 1 * * *",
			after:      time.Date(2024, time.November, 3, 5, 45, 0, 0, time.UTC),
			want:       time.Date(2024, time.November, 3, 6, 30, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NextScheduleTime(test.expression, "America/New_York", test.after)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("NextScheduleTime() = %s, want %s", got, test.want)
			}
		})
	}
}

type countingSchedule struct {
	cron.Schedule
	calls int
}

func (schedule *countingSchedule) Next(after time.Time) time.Time {
	schedule.calls++
	return schedule.Schedule.Next(after)
}

func TestPreviousScheduleTimeUsesBoundedSearch(t *testing.T) {
	schedule, err := ParseSchedule("0 0 29 2 *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingSchedule{Schedule: schedule}
	before := time.Date(2028, time.February, 29, 0, 0, 0, 0, time.UTC)
	got, err := PreviousScheduleTime(counted, before)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("PreviousScheduleTime() = %s, want %s", got, want)
	}
	if counted.calls > 100 {
		t.Fatalf("PreviousScheduleTime() called Next %d times, want at most 100", counted.calls)
	}
}

func TestPreviousScheduleTimeAcrossDST(t *testing.T) {
	for _, test := range []struct {
		name, expression string
		before, want     time.Time
	}{
		{"spring skipped activation", "30 2 * * *", time.Date(2024, 3, 11, 6, 30, 0, 0, time.UTC), time.Date(2024, 3, 9, 7, 30, 0, 0, time.UTC)},
		{"fall repeated activation", "30 1 * * *", time.Date(2024, 11, 3, 6, 30, 0, 0, time.UTC), time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC)},
	} {
		t.Run(test.name, func(t *testing.T) {
			schedule, err := ParseSchedule(test.expression, "America/New_York")
			if err != nil {
				t.Fatal(err)
			}
			previous, err := PreviousScheduleTime(schedule, test.before)
			if err != nil || !previous.Equal(test.want) {
				t.Fatalf("previous activation=%s %v, want %s", previous, err, test.want)
			}
		})
	}
}

func TestScheduleTimeRejectsZeroAndSchedulesWithoutActivations(t *testing.T) {
	if _, err := NextScheduleTime("0 0 * * *", "UTC", time.Time{}); err == nil {
		t.Fatal("NextScheduleTime accepted a zero start time")
	}
	if _, err := NextScheduleTime("0 0 31 2 *", "UTC", time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("NextScheduleTime accepted a schedule without an activation")
	}
	if _, err := PreviousScheduleTime(nil, time.Now()); err == nil {
		t.Fatal("PreviousScheduleTime accepted a nil schedule")
	}
	if _, err := PreviousScheduleTime(cron.ConstantDelaySchedule{}, time.Time{}); err == nil {
		t.Fatal("PreviousScheduleTime accepted a zero end time")
	}

	schedule, err := ParseSchedule("0 0 31 2 *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PreviousScheduleTime(schedule, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("PreviousScheduleTime accepted a schedule without an activation")
	}
}
