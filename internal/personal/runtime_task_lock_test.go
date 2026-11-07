package personal

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type unwindingTaskStore struct {
	Repository
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (s *unwindingTaskStore) ReadCheckpoints(ctx context.Context, runID int64) ([]ReadCheckpoint, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		<-ctx.Done()
		<-s.release // The cancelled stage still owns its workspace while unwinding.
		return nil, ctx.Err()
	}
	return s.Repository.ReadCheckpoints(ctx, runID)
}

func TestRuntimeWorkspaceWaitDoesNotChargeBudget(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		name := "resume"
		if cancelWaiting {
			name = "cancel_waiting"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			const budget = 300 * time.Millisecond
			s := testStore(t, filepath.Join(t.TempDir(), "serial.db"), budget)
			repo := &unwindingTaskStore{Repository: s, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			var workers sync.WaitGroup
			t.Cleanup(func() {
				cancel()
				release.Do(func() { close(repo.release) })
				workers.Wait()
			})
			event := openUserEvent(t, s, 1, 1, "write report")
			task, first := createTask(t, s, event, "report", "report", "Write report", "")
			finishEvent(t, s, event, TurnOutput{}, nil)
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText("Other task complete."), responseText("Report complete.")}}
			runtime := testRuntime(repo)
			runtime.Execute = TaskExecutor(readingEngine(fixture, nil), repo, t.TempDir())
			start := func() <-chan error {
				done := make(chan error, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					worked, err := runtime.task(ctx)
					if err == nil && !worked {
						err = errors.New("no task was claimed")
					}
					done <- err
				}()
				return done
			}
			firstDone := start()
			receive(t, repo.entered)
			revision := openUserEvent(t, s, 2, 2, "revise report")
			revised, queued, err := s.ReviseTask(ctx, revision.ID, "revise", task.ID, task.Revision, TaskInput{Goal: "Revise report"})
			if err != nil {
				t.Fatal(err)
			}
			_, other := createTask(t, s, revision, "other", "other", "Independent task", "")
			finishEvent(t, s, revision, TurnOutput{}, nil)
			if err := checkTaskRun(ctx, s, first); err == nil {
				t.Fatal("cancelled stage still has file access")
			}
			replacementDone := start()
			eventually(t, func() bool {
				run, err := s.Run(ctx, queued.ID)
				return err == nil && run.Status == RunRunning
			})
			if cancelWaiting {
				cancelEvent := openUserEvent(t, s, 3, 3, "cancel report")
				if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, revised.Revision); err != nil {
					t.Fatal(err)
				}
				finishEvent(t, s, cancelEvent, TurnOutput{}, nil)
				if err := receive(t, replacementDone); err != nil {
					t.Fatal(err)
				}
			} else {
				time.Sleep(2 * budget)
				select {
				case err := <-replacementDone:
					t.Fatalf("waiting stage finished before workspace was released: %v", err)
				default:
				}
			}
			after, err := s.Task(ctx, task.ID)
			if err != nil || after.UsedMS != revised.UsedMS {
				t.Fatalf("workspace wait charged budget: task=%+v before=%d err=%v", after, revised.UsedMS, err)
			}
			waiting, err := s.Run(ctx, queued.ID)
			if err != nil || waiting.ElapsedMS != 0 {
				t.Fatalf("workspace wait charged run: %+v, %v", waiting, err)
			}
			if err := receive(t, start()); err != nil {
				t.Fatal(err)
			}
			independent, err := s.Run(ctx, other.ID)
			if err != nil || independent.Status != RunCompleted {
				t.Fatalf("another task was blocked: %+v, %v", independent, err)
			}
			release.Do(func() { close(repo.release) })
			if err := receive(t, firstDone); err != nil {
				t.Fatal(err)
			}
			if !cancelWaiting {
				if err := receive(t, replacementDone); err != nil {
					t.Fatal(err)
				}
				completed, err := s.Run(ctx, queued.ID)
				if err != nil || completed.Status != RunCompleted {
					t.Fatalf("replacement did not complete: %+v, %v", completed, err)
				}
			}
			if _, err := runtime.Execute(ctx, first, nil); err == nil {
				t.Fatal("stale run restarted after the workspace was released")
			}
		})
	}
}
