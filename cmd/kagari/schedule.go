package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"

	"kagari/internal/config"
	"kagari/internal/personal"
	"kagari/internal/personal/sqlite"
)

func inspectSchedules(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("schedules", flag.ContinueOnError)
	before := flags.Int64("before", 0, "show schedules with IDs below this value")
	limit := flags.Int("limit", 20, "number of schedules (1..100)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: kagari schedules [-before ID] [-limit N] [ID]")
	}
	if err := validateSchedulePage(*before, *limit); err != nil {
		return err
	}
	var id int64
	if flags.NArg() == 1 {
		var err error
		id, err = positiveScheduleID(flags.Arg(0))
		if err != nil {
			return err
		}
	}
	s, err := sqlite.OpenExisting(ctx, cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	var result any
	if id > 0 {
		schedule, err := s.Schedule(ctx, id)
		if err == nil && schedule == nil {
			err = fmt.Errorf("schedule #%d does not exist", id)
		}
		if err != nil {
			return err
		}
		suggestions, err := s.ScheduleSuggestions(ctx, id)
		if err != nil {
			return err
		}
		occurrences, err := s.ScheduleOccurrences(ctx, id, *before, *limit)
		if err != nil {
			return err
		}
		result = struct {
			*personal.Schedule
			Suggestions []personal.ScheduleSuggestion `json:"suggestions"`
			Occurrences []personal.ScheduleOccurrence `json:"occurrences"`
		}{schedule, suggestions, occurrences}
	} else {
		// CLI inspection includes cancelled definitions so their retained history remains discoverable.
		result, err = s.Schedules(ctx, true, *before, *limit)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(result)
}

func inspectScheduleHistory(ctx context.Context, cfg config.Config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("schedule-history", flag.ContinueOnError)
	before := flags.Int64("before", 0, "show occurrences with IDs below this value")
	limit := flags.Int("limit", 20, "number of occurrences (1..100)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: kagari schedule-history [-before ID] [-limit N] ID")
	}
	if err := validateSchedulePage(*before, *limit); err != nil {
		return err
	}
	id, err := positiveScheduleID(flags.Arg(0))
	if err != nil {
		return err
	}
	s, err := sqlite.OpenExisting(ctx, cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer s.Close()
	occurrences, err := s.ScheduleOccurrences(ctx, id, *before, *limit)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(occurrences)
}

func validateSchedulePage(before int64, limit int) error {
	if before < 0 || limit < 1 || limit > 100 {
		return errors.New("before must be non-negative and limit must be between 1 and 100")
	}
	return nil
}

func positiveScheduleID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("schedule ID must be a positive integer")
	}
	return id, nil
}
