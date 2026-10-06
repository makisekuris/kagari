package digest

import (
	"fmt"
	"time"

	"kagari/internal/config"
	"kagari/internal/domain"
)

func PreviousWeek(now time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	weekStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	daysSinceMonday := (int(weekStart.Weekday()) + 6) % 7
	// 用本地日历日 AddDate 得到 Mon 00:00 边界；整周可能因 DST 不是 168 小时。
	end = weekStart.AddDate(0, 0, -daysSinceMonday)
	start = end.AddDate(0, 0, -7)
	return start, end
}

// Window returns the seven calendar days ending at cutoff, preserving its local wall time.
func Window(cutoff time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	end = cutoff
	start = cutoff.In(loc).AddDate(0, 0, -7)
	return start, end
}

func Due(now, enabledAt time.Time, cfg config.Weekly) ([]domain.DigestRequest, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Weekday < 0 || cfg.Weekday > 6 {
		return nil, fmt.Errorf("weekly.weekday must be 0..6")
	}
	clock, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return nil, fmt.Errorf("weekly.time must be HH:MM: %w", err)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("weekly.timezone: %w", err)
	}
	if !enabledAt.Before(now) {
		return nil, nil
	}

	// enabledAt 作为排程 cursor：严格排除启用前/恰好启用时的触发，now 命中的触发则包含。
	localEnabled := enabledAt.In(loc)
	day := time.Date(localEnabled.Year(), localEnabled.Month(), localEnabled.Day(), 0, 0, 0, 0, loc)
	daysUntil := (cfg.Weekday - int(day.Weekday()) + 7) % 7
	day = day.AddDate(0, 0, daysUntil)
	hour, minute, _ := clock.Clock()
	trigger := scheduledAt(day, hour, minute, loc)
	if !trigger.After(enabledAt) {
		day = day.AddDate(0, 0, 7)
		trigger = scheduledAt(day, hour, minute, loc)
	}

	requests := make([]domain.DigestRequest, 0)
	for !trigger.After(now) {
		start, end := Window(trigger, loc)
		requests = append(requests, domain.DigestRequest{Version: domain.DigestVersion, Start: start, End: end})
		day = day.AddDate(0, 0, 7)
		trigger = scheduledAt(day, hour, minute, loc)
	}
	return requests, nil
}

func scheduledAt(day time.Time, hour, minute int, loc *time.Location) time.Time {
	date := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	for {
		local := date.In(loc)
		if local.Year() == day.Year() && local.Month() == day.Month() && local.Day() == day.Day() {
			break
		}
		if local.Year() > day.Year() || local.Year() == day.Year() && (local.Month() > day.Month() || local.Month() == day.Month() && local.Day() > day.Day()) {
			return date
		}
		date = date.Add(time.Minute)
	}
	targetMinute := hour*60 + minute
	// 春季目标时刻不存在时取当日首次有效的后续分钟；秋季重复时刻取第一次出现。
	for instant := date; ; instant = instant.Add(time.Minute) {
		local := instant.In(loc)
		if local.Year() != day.Year() || local.Month() != day.Month() || local.Day() != day.Day() {
			return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
		}
		wallMinute := local.Hour()*60 + local.Minute()
		if wallMinute >= targetMinute {
			return instant
		}
	}
}
