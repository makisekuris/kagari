package personal

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const defaultScheduleTimezone = "Asia/Shanghai"

var scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func parseSchedule(expression, timezone string) (cron.Schedule, error) {
	if timezone == "" {
		timezone = defaultScheduleTimezone
	}
	if timezone == "Local" {
		return nil, fmt.Errorf("schedule timezone must be an IANA location, not Local")
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule timezone %q: %w", timezone, err)
	}
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		return nil, fmt.Errorf("schedule expression must not contain a timezone")
	}

	schedule, err := scheduleParser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule expression: %w", err)
	}
	schedule.(*cron.SpecSchedule).Location = location
	return schedule, nil
}

func nextScheduleTime(expression, timezone string, after time.Time) (time.Time, error) {
	if after.IsZero() {
		return time.Time{}, fmt.Errorf("schedule start time must not be zero")
	}
	schedule, err := parseSchedule(expression, timezone)
	if err != nil {
		return time.Time{}, err
	}
	next := schedule.Next(after.UTC())
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("schedule has no activation within five years")
	}
	return next.UTC(), nil
}

func previousScheduleTime(schedule cron.Schedule, before time.Time) (time.Time, error) {
	if schedule == nil {
		return time.Time{}, fmt.Errorf("schedule must not be nil")
	}
	if before.IsZero() {
		return time.Time{}, fmt.Errorf("schedule end time must not be zero")
	}

	// shortcut: five-field cron has no year field; raise this bound if that changes.
	const maxLookback = 10 * 365 * 24 * time.Hour
	beforeUTC := before.UTC()
	upper := beforeUTC
	lookback := time.Minute
	var lower time.Time
	for {
		lower = beforeUTC.Add(-lookback)
		next := schedule.Next(lower)
		if !next.IsZero() && next.Before(beforeUTC) {
			break
		}
		if lookback == maxLookback {
			return time.Time{}, fmt.Errorf("schedule has no previous activation within ten years")
		}
		if lookback > maxLookback/2 {
			lookback = maxLookback
		} else {
			lookback *= 2
		}
	}

	precedes := func(at time.Time) bool {
		next := schedule.Next(at)
		return !next.IsZero() && next.Before(beforeUTC)
	}
	for upper.Sub(lower) > time.Nanosecond {
		middle := lower.Add(upper.Sub(lower) / 2)
		if precedes(middle) {
			lower = middle
		} else {
			upper = middle
		}
	}

	previous := schedule.Next(lower)
	if previous.IsZero() || !previous.Before(beforeUTC) {
		return time.Time{}, fmt.Errorf("schedule has no previous activation within ten years")
	}
	return previous.UTC(), nil
}
