package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"kagari/internal/telegram"
)

func Run(ctx context.Context, w *Worker) error {
	if err := w.Config.Validate(true, true); err != nil {
		return err
	}
	client, err := telegram.New(w.Config.Telegram, w.Store, w.Engine.Prepare, w.Log)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	failures := make(chan error, 3)
	start := func(fn func(context.Context) error) {
		group.Add(1)
		go func() {
			defer group.Done()
			err := fn(ctx)
			if err != nil && ctx.Err() == nil {
				select {
				case failures <- err:
				default:
				}
				cancel()
			}
		}()
	}
	start(client.Poll)
	start(func(ctx context.Context) error { return deliver(ctx, w, client.Send) })
	start(func(ctx context.Context) error { return work(ctx, w) })
	<-ctx.Done()
	group.Wait()
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
}

func work(ctx context.Context, w *Worker) error {
	// ponytail: one worker; commands wait behind the current analysis. Separate
	// command processing only if interactive latency becomes a problem.
	nextSchedule := time.Time{}
	for ctx.Err() == nil {
		if now := time.Now(); !now.Before(nextSchedule) {
			if err := schedule(ctx, w, now); err != nil {
				return fmt.Errorf("weekly scheduling: %w", err)
			}
			nextSchedule = now.Add(time.Minute)
		}
		job, err := w.Store.ClaimJob(ctx)
		if err != nil {
			return err
		}
		if job == nil {
			if !pause(ctx, time.Second) {
				break
			}
			continue
		}
		if err := w.Process(ctx, job); err != nil && ctx.Err() == nil {
			persisted, stateErr := w.Store.Job(ctx, job.ID)
			if stateErr != nil {
				return stateErr
			}
			if persisted == nil || persisted.Status == "processing" {
				return errors.New("could not persist job outcome; restart to recover the task")
			}
		}
	}
	return nil
}
