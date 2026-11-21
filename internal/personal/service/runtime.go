package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

// Runtime owns scheduling. Model tools persist work without waiting for its execution.
type Runtime struct {
	Timezone        string
	Store           Repository
	Think           personal.ThinkFunc
	Execute         personal.ExecuteFunc
	ReviewMemory    personal.ReviewMemoryFunc
	PresentReply    func(context.Context, personal.Event, string) (string, error)
	TaskConcurrency int
	Poll            func(context.Context) error
	Ready           func(context.Context) error
	Prepare         func(string) ([]string, error)
	Send            func(context.Context, channel.Content) (channel.Receipt, error)
	Log             *zap.Logger
	idleDelay       time.Duration
	taskMu          sync.Mutex
	activeTasks     map[int64]chan struct{}
}

// Run requires the caller to hold the instance lock before startup recovery.
func (r *Runtime) Run(ctx context.Context) error {
	if r.Store == nil || r.Think == nil || r.Execute == nil || r.ReviewMemory == nil || r.PresentReply == nil || r.Prepare == nil || r.Send == nil {
		return errors.New("personal runtime requires store, thinker, task executor, memory reviewer, reply presenter and delivery")
	}
	if r.TaskConcurrency == 0 {
		r.TaskConcurrency = 4
	}
	if r.TaskConcurrency < 1 {
		return errors.New("personal task concurrency must be positive")
	}
	if r.Log == nil {
		r.Log = zap.NewNop()
	}
	if r.idleDelay <= 0 {
		r.idleDelay = 200 * time.Millisecond
	}
	ctx = logging.WithLogger(ctx, r.Log)
	if err := r.Store.Recover(ctx); err != nil {
		if ctx.Err() != nil && onlyCancellationError(err, ctx.Err()) {
			return nil
		}
		return err
	}
	r.Log.Info("service.ready", zap.Int("task_concurrency", r.TaskConcurrency))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	failures := make(chan error, r.TaskConcurrency+5)
	start := func(name string, fn func(context.Context) error) {
		group.Add(1)
		go func() {
			defer group.Done()
			err := fn(ctx)
			if err == nil {
				if ctx.Err() != nil {
					return
				}
				err = errors.New("personal worker stopped unexpectedly")
			}
			if ctx.Err() != nil && !isDeliveryPersistenceError(err) && onlyCancellationError(err, ctx.Err()) {
				return
			}
			r.Log.Error("service.worker.failed", append(logging.ErrorFields(err), zap.String("worker", name))...)
			failures <- err
			cancel()
		}()
	}
	start("schedule", func(ctx context.Context) error { return r.loop(ctx, r.schedule) })
	start("conversation", func(ctx context.Context) error { return r.loop(ctx, r.turn) })
	for i := 0; i < r.TaskConcurrency; i++ {
		start("task", func(ctx context.Context) error { return r.loop(ctx, r.task) })
	}
	start("memory", func(ctx context.Context) error { return r.loop(ctx, r.memory) })
	start("delivery", func(ctx context.Context) error { return r.loop(ctx, r.deliver) })
	if r.Poll != nil {
		start("channel", r.Poll)
	}
	<-ctx.Done()
	group.Wait()
	close(failures)
	var workerErrors []error
	for err := range failures {
		workerErrors = append(workerErrors, err)
	}
	return errors.Join(workerErrors...)
}

func isDeliveryPersistenceError(err error) bool {
	var persistErr *deliveryPersistenceError
	return errors.As(err, &persistErr)
}

func onlyCancellationError(err, cancellation error) bool {
	if err == nil || cancellation == nil {
		return false
	}
	if err == cancellation {
		return true
	}
	// Cancelling a context during an active SQLite statement may surface the
	// driver's interrupt instead of ctx.Err(). Keep checking joined siblings.
	if dbErr, ok := err.(*sqlitedriver.Error); ok {
		return dbErr.Code() == sqlite3.SQLITE_INTERRUPT
	}
	if multiple, ok := err.(interface{ Unwrap() []error }); ok {
		unwrapped := multiple.Unwrap()
		if len(unwrapped) == 0 {
			return false
		}
		for _, child := range unwrapped {
			if !onlyCancellationError(child, cancellation) {
				return false
			}
		}
		return true
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return onlyCancellationError(single.Unwrap(), cancellation)
	}
	return false
}

func (r *Runtime) loop(ctx context.Context, step func(context.Context) (bool, error)) error {
	for ctx.Err() == nil {
		worked, err := step(ctx)
		if err != nil {
			return err
		}
		if !worked && !wait(ctx, r.idleDelay) {
			break
		}
	}
	return nil
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
