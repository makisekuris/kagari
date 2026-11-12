package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

// Runtime owns scheduling. Model tools persist work without waiting for its execution.
type Runtime struct {
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
		return err
	}
	r.Log.Info("service.ready", zap.Int("task_concurrency", r.TaskConcurrency))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	failures := make(chan error, r.TaskConcurrency+6)
	start := func(name string, fn func(context.Context) error) {
		group.Add(1)
		go func() {
			defer group.Done()
			err := fn(ctx)
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("personal worker stopped unexpectedly")
				}
				r.Log.Error("service.worker.failed", append(logging.ErrorFields(err), zap.String("worker", name))...)
				failures <- err
				cancel()
			}
		}()
	}
	start("schedule", func(ctx context.Context) error { return r.loop(ctx, r.schedule) })
	start("conversation", func(ctx context.Context) error { return r.loop(ctx, r.turn) })
	start("reply", func(ctx context.Context) error { return r.loop(ctx, r.waitingReply) })
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
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
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
