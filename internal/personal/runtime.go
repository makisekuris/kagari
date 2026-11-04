package personal

import (
	"context"
	"errors"
	"sync"
	"time"

	"kagari/internal/logging"

	"go.uber.org/zap"
)

// Runtime owns scheduling. Model tools persist work without waiting for its execution.
type Runtime struct {
	Store           Repository
	Think           ThinkFunc
	Execute         ExecuteFunc
	ReviewMemory    ReviewMemoryFunc
	TaskConcurrency int
	Poll            func(context.Context) error
	Prepare         func(string) ([]string, error)
	Send            func(context.Context, string) (int64, error)
	Log             *zap.Logger
	idleDelay       time.Duration
}

// Run requires the caller to hold the instance lock before startup recovery.
func (r *Runtime) Run(ctx context.Context) error {
	if r.Store == nil || r.Think == nil || r.Execute == nil || r.ReviewMemory == nil || r.Prepare == nil || r.Send == nil {
		return errors.New("personal runtime requires store, thinker, task executor, memory reviewer and delivery")
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
	failures := make(chan error, r.TaskConcurrency+4)
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
	start("conversation", func(ctx context.Context) error { return r.loop(ctx, r.turn) })
	for i := 0; i < r.TaskConcurrency; i++ {
		start("task", func(ctx context.Context) error { return r.loop(ctx, r.task) })
	}
	start("memory", func(ctx context.Context) error { return r.loop(ctx, r.memory) })
	start("delivery", func(ctx context.Context) error { return r.loop(ctx, r.deliver) })
	if r.Poll != nil {
		start("telegram", r.Poll)
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

func clip(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "\n（节选；完整内容可通过工具查询。）"
}
