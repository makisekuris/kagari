package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"kagari/internal/app"
	"kagari/internal/digest"
	"kagari/internal/domain"
)

func runDigest(ctx context.Context, worker *app.Worker, args []string, out io.Writer) error {
	request, err := digestRequest(args, time.Now(), worker.Config.Weekly.Timezone)
	if err != nil {
		return err
	}
	id, _, err := worker.EnqueueDigest(ctx, request, nil)
	if err != nil {
		return err
	}
	job, err := worker.ProcessJob(ctx, id)
	if err != nil {
		return err
	}
	var report digest.Report
	if err := json.Unmarshal(job.Result, &report); err != nil {
		return err
	}
	_, err = fmt.Fprint(out, digest.Render(report))
	return err
}

func digestRequest(args []string, now time.Time, timezone string) (request domain.DigestRequest, err error) {
	flags := flag.NewFlagSet("digest", flag.ContinueOnError)
	startFlag := flags.String("start", "", "inclusive receipt date YYYY-MM-DD")
	endFlag := flags.String("end", "", "exclusive receipt date YYYY-MM-DD")
	cutoffFlag := flags.String("cutoff", "", "exclusive cutoff timestamp RFC3339; defaults to now")
	user := flags.Int64("user", 0, "archive owner user ID")
	if err := flags.Parse(args); err != nil {
		return request, err
	}
	if len(flags.Args()) != 0 {
		return request, errors.New("unexpected digest arguments")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return request, err
	}
	if *cutoffFlag != "" {
		if *startFlag != "" || *endFlag != "" {
			return request, errors.New("cutoff cannot be combined with start/end dates")
		}
		now, err = time.Parse(time.RFC3339Nano, *cutoffFlag)
		if err != nil {
			return request, fmt.Errorf("cutoff must be RFC3339: %w", err)
		}
	}
	start, end := digest.Window(now, loc)
	if *startFlag != "" || *endFlag != "" {
		if *startFlag == "" || *endFlag == "" {
			return request, errors.New("start and end must be provided together")
		}
		start, err = time.ParseInLocation("2006-01-02", *startFlag, loc)
		if err != nil {
			return request, err
		}
		end, err = time.ParseInLocation("2006-01-02", *endFlag, loc)
		if err != nil {
			return request, err
		}
	}
	if !start.Before(end) {
		return request, errors.New("start must be before cutoff")
	}
	return domain.DigestRequest{UserID: *user, Start: start.UTC(), End: end.UTC(), Version: domain.DigestVersion}, nil
}
