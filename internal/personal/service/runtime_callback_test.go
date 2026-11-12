package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

type callbackRuntimeStore struct {
	Repository
	firstEvent   *personal.Event
	stateErr     error
	stateFailed  bool
	historyCalls int
	accountCalls int
}

func (s *callbackRuntimeStore) ClaimEvent(ctx context.Context) (*personal.Event, error) {
	if s.firstEvent != nil {
		event := *s.firstEvent
		s.firstEvent = nil
		return &event, nil
	}
	return s.Repository.ClaimEvent(ctx)
}

func (s *callbackRuntimeStore) TaskCallbackState(ctx context.Context, eventID int64) (personal.TaskCallbackState, error) {
	if s.stateErr != nil && !s.stateFailed {
		s.stateFailed = true
		return "", s.stateErr
	}
	return s.Repository.TaskCallbackState(ctx, eventID)
}

func (s *callbackRuntimeStore) History(ctx context.Context, eventID int64, limit int) ([]personal.Message, error) {
	s.historyCalls++
	return s.Repository.History(ctx, eventID, limit)
}

func (s *callbackRuntimeStore) AccountEvent(ctx context.Context, eventID int64, elapsed time.Duration) error {
	s.accountCalls++
	return s.Repository.AccountEvent(ctx, eventID, elapsed)
}

type racingTaskMutationStore struct {
	Repository
	firstEvent *personal.Event
	callbackID int64
	mutate     func()
}

func (s *racingTaskMutationStore) ClaimEvent(ctx context.Context) (*personal.Event, error) {
	if s.firstEvent != nil {
		event := *s.firstEvent
		s.firstEvent = nil
		return &event, nil
	}
	return s.Repository.ClaimEvent(ctx)
}

func (s *racingTaskMutationStore) ContinueTask(ctx context.Context, eventID int64, key string, taskID, revision int64, step string, extend bool) (personal.Task, personal.Run, error) {
	if eventID == s.callbackID && s.mutate != nil {
		mutate := s.mutate
		s.mutate = nil
		mutate()
	}
	return s.Repository.ContinueTask(ctx, eventID, key, taskID, revision, step, extend)
}

func (s *racingTaskMutationStore) SettleTask(ctx context.Context, eventID, taskID, revision int64, settlement personal.TaskSettlement) (personal.Task, error) {
	if eventID == s.callbackID && s.mutate != nil {
		mutate := s.mutate
		s.mutate = nil
		mutate()
	}
	return s.Repository.SettleTask(ctx, eventID, taskID, revision, settlement)
}

func newRunningTaskCallback(t *testing.T) (*sqlite.Store, personal.Task, personal.Run, personal.Event) {
	t.Helper()
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	user := testkit.OpenUserEvent(t, store, 1, 1, "start research")
	task, firstRun := testkit.CreateTask(t, store, user, "create", "research", "research the topic", "")
	testkit.FinishEvent(t, store, user, personal.TurnOutput{}, nil)
	claimed, err := store.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != firstRun.ID {
		t.Fatalf("claim first run = (%+v, %v), want run %d", claimed, err, firstRun.ID)
	}
	if err := store.FinishStage(ctx, firstRun.ID, personal.StageResult{Summary: "first stage result"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask || callback.TaskID != task.ID {
		t.Fatalf("claim task callback = (%+v, %v)", callback, err)
	}
	return store, task, firstRun, *callback
}

func continueTaskFromNextUserEvent(t *testing.T, store *sqlite.Store, task personal.Task, updateID, messageID int64, key string) (personal.Event, personal.Run) {
	t.Helper()
	ctx := context.Background()
	user := testkit.OpenUserEvent(t, store, updateID, messageID, "continue the same task")
	_, run, err := store.ContinueTask(ctx, user.ID, key, task.ID, task.Revision, "continue from the saved result", false)
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, user, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
	return user, run
}

func completeStaleCallbackTurn(t *testing.T, store *sqlite.Store, callback personal.Event) {
	t.Helper()
	wrapped := &callbackRuntimeStore{Repository: store, firstEvent: &callback}
	runtime := testRuntime(wrapped)
	thinkCalls, presentationCalls := 0, 0
	runtime.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		thinkCalls++
		return personal.TurnOutput{Text: "obsolete output"}, nil
	}
	runtime.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		presentationCalls++
		return "obsolete output", nil
	}
	worked, err := runtime.turn(context.Background())
	if err != nil || !worked {
		t.Fatalf("stale callback turn = (%t, %v)", worked, err)
	}
	if thinkCalls != 0 || presentationCalls != 0 || wrapped.historyCalls != 0 || wrapped.accountCalls != 0 {
		t.Fatalf("stale callback reached work: think=%d presentation=%d history=%d accounting=%d", thinkCalls, presentationCalls, wrapped.historyCalls, wrapped.accountCalls)
	}
	state, err := store.TaskCallbackState(context.Background(), callback.ID)
	if err != nil || state != personal.TaskCallbackStale {
		t.Fatalf("callback state after completion = (%q, %v), want stale", state, err)
	}
	snapshot, err := store.Diagnostic(context.Background(), callback.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range snapshot.Events {
		if event.ID == callback.ID && event.Status != personal.EventCompleted {
			t.Fatalf("old callback status = %q, want completed", event.Status)
		}
	}
	for _, delivery := range snapshot.Deliveries {
		if delivery.EventID == callback.ID {
			t.Fatalf("stale callback queued delivery: %+v", delivery)
		}
	}
}

func TestRuntimeOldCallbackCannotTouchSameRevisionContinuation(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "replacement_running"
		if completed {
			name = "replacement_completed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, task, _, callback := newRunningTaskCallback(t)
			_, nextRun := continueTaskFromNextUserEvent(t, store, task, 2, 2, "continue-b")
			claimed, err := store.ClaimRun(ctx, personal.RunTask)
			if err != nil || claimed == nil || claimed.ID != nextRun.ID {
				t.Fatalf("claim replacement run = (%+v, %v), want %d", claimed, err, nextRun.ID)
			}
			if completed {
				if err := store.FinishStage(ctx, nextRun.ID, personal.StageResult{Summary: "replacement result"}, ""); err != nil {
					t.Fatal(err)
				}
				replacementCallback, err := store.ClaimEvent(ctx)
				if err != nil || replacementCallback == nil || replacementCallback.Kind != personal.EventTask {
					t.Fatalf("claim replacement callback = (%+v, %v)", replacementCallback, err)
				}
				if _, err := store.SettleTask(ctx, replacementCallback.ID, task.ID, task.Revision, personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "replacement complete", Delivery: personal.DeliveryDraft{Text: "replacement complete"}}); err != nil {
					t.Fatal(err)
				}
				testkit.FinishEvent(t, store, *replacementCallback, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
			}
			beforeTask, err := store.Task(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeRun, err := store.Run(ctx, nextRun.ID)
			if err != nil {
				t.Fatal(err)
			}
			completeStaleCallbackTurn(t, store, callback)
			afterTask, err := store.Task(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterRun, err := store.Run(ctx, nextRun.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeRun, afterRun) {
				t.Fatalf("stale callback changed replacement: task before/after=%+v/%+v run before/after=%+v/%+v", beforeTask, afterTask, beforeRun, afterRun)
			}
		})
	}
}

func TestRuntimeOldCallbackIsStaleAfterRevisionOrCancellation(t *testing.T) {
	for _, revise := range []bool{true, false} {
		name := "cancelled"
		if revise {
			name = "revised"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, task, _, callback := newRunningTaskCallback(t)
			user := testkit.OpenUserEvent(t, store, 2, 2, "change task")
			if revise {
				if _, _, err := store.ReviseTask(ctx, user.ID, "revise", task.ID, task.Revision, personal.TaskInput{Goal: "a revised research goal"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := store.CancelTask(ctx, user.ID, task.ID, task.Revision); err != nil {
					t.Fatal(err)
				}
			}
			testkit.FinishEvent(t, store, user, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
			beforeTask, err := store.Task(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			completeStaleCallbackTurn(t, store, callback)
			afterTask, err := store.Task(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeTask, afterTask) {
				t.Fatalf("stale callback changed task: before=%+v after=%+v", beforeTask, afterTask)
			}
		})
	}
}

func TestRuntimeCallbackBecomingStaleDuringThinkIsCompletedWithoutPresentation(t *testing.T) {
	ctx := context.Background()
	store, task, _, callback := newRunningTaskCallback(t)
	before, err := store.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &callbackRuntimeStore{Repository: store, firstEvent: &callback}
	runtime := testRuntime(wrapper)
	runtime.idleDelay = time.Hour
	thinkCalls, presentationCalls := 0, 0
	runtime.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		thinkCalls++
		continueTaskFromNextUserEvent(t, store, task, 2, 2, "continue-during-think")
		return personal.TurnOutput{Text: "old result must not be shown", TaskID: task.ID}, nil
	}
	runtime.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		presentationCalls++
		return "old result must not be shown", nil
	}
	worked, err := runtime.turn(ctx)
	if err != nil || !worked {
		t.Fatalf("callback turn = (%t, %v)", worked, err)
	}
	if thinkCalls != 1 || presentationCalls != 0 {
		t.Fatalf("callback calls = think:%d presentation:%d", thinkCalls, presentationCalls)
	}
	after, err := store.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UsedMS != before.UsedMS || after.Status != personal.TaskQueued {
		t.Fatalf("stale callback altered budget or replacement status: before=%+v after=%+v", before, after)
	}
	snapshot, err := store.Diagnostic(ctx, callback.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range snapshot.Deliveries {
		if delivery.EventID == callback.ID {
			t.Fatalf("stale callback queued delivery: %+v", delivery)
		}
	}
}

func TestRuntimeStaleFallbackTaskMutationsSilentlyCompleteCallback(t *testing.T) {
	for _, path := range []string{"continue", "settle", "recover"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			store, task, _, callback := newRunningTaskCallback(t)
			if path == "continue" {
				pending := testkit.OpenUserEvent(t, store, 2, 2, "add more material")
				if _, err := store.AddTaskInputs(ctx, pending.ID, "add-input", task.ID, task.Revision, []int64{pending.ID}); err != nil {
					t.Fatal(err)
				}
				testkit.FinishEvent(t, store, pending, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
			}
			if path == "recover" {
				if err := store.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				reclaimed, err := store.ClaimEvent(ctx)
				if err != nil || reclaimed == nil || reclaimed.ID != callback.ID || !reclaimed.Interrupted {
					t.Fatalf("reclaim interrupted callback = (%+v, %v)", reclaimed, err)
				}
				callback = *reclaimed
			}
			mutation := func() {
				continueTaskFromNextUserEvent(t, store, task, 3, 3, "replacement-before-stale-mutation")
			}
			wrapper := &racingTaskMutationStore{
				Repository: store,
				firstEvent: &callback,
				callbackID: callback.ID,
				mutate:     mutation,
			}
			runtime := testRuntime(wrapper)
			runtime.idleDelay = time.Hour
			presenterCalls := 0
			runtime.PresentReply = func(context.Context, personal.Event, string) (string, error) {
				presenterCalls++
				return "stale output", nil
			}
			runtime.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
				return personal.TurnOutput{Text: "intermediate result", TaskID: task.ID}, nil
			}
			worked, err := runtime.turn(ctx)
			if err != nil || !worked {
				t.Fatalf("callback turn = (%t, %v)", worked, err)
			}
			if presenterCalls != 0 {
				t.Fatalf("presented stale output %d times", presenterCalls)
			}
			if wrapper.mutate != nil {
				t.Fatal("expected stale fallback mutation was not reached")
			}
			state, err := store.TaskCallbackState(ctx, callback.ID)
			if err != nil || state != personal.TaskCallbackStale {
				t.Fatalf("callback state = (%q,%v), want stale", state, err)
			}
			current, err := store.Task(ctx, task.ID)
			if err != nil || current.Status != personal.TaskQueued {
				t.Fatalf("valid continuation was lost: task=%+v err=%v", current, err)
			}
		})
	}
}

func TestRuntimeCallbackStateDatabaseErrorPropagatesAndNextUserEventRuns(t *testing.T) {
	ctx := context.Background()
	store, _, _, callback := newRunningTaskCallback(t)
	databaseErr := errors.New("injected callback state database failure")
	wrapper := &callbackRuntimeStore{Repository: store, firstEvent: &callback, stateErr: databaseErr}
	runtime := testRuntime(wrapper)
	var userThinkCalls int
	runtime.Think = func(_ context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if input.Event.Kind != personal.EventUser {
			t.Fatalf("unexpected Think event after query failure: %+v", input.Event)
		}
		userThinkCalls++
		return personal.TurnOutput{Text: "following user event processed"}, nil
	}
	if worked, err := runtime.turn(ctx); !worked || !errors.Is(err, databaseErr) {
		t.Fatalf("callback state failure turn = (%t, %v), want injected database error", worked, err)
	}
	userEventID := testkit.AcceptInput(t, store, "2", testkit.SourceInput("2", "handle this next request"))
	if worked, err := runtime.turn(ctx); err != nil || !worked {
		t.Fatalf("following user turn = (%t, %v)", worked, err)
	}
	if userThinkCalls != 1 {
		t.Fatalf("following user Think calls = %d, want 1", userThinkCalls)
	}
	message := testkit.AssistantMessage(t, store, userEventID)
	if message.Text != "following user event processed" {
		t.Fatalf("following user output = %q", message.Text)
	}
}
