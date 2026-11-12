package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

type controlledDeliveryStore struct {
	Repository
	entered   chan struct{}
	release   chan struct{}
	updateErr error
	once      sync.Once
}

func (s *controlledDeliveryStore) UpdateDelivery(ctx context.Context, deliveryID int64, status string, receipt channel.Receipt, detail string, nextAttempt time.Time) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		if s.updateErr != nil {
			return s.updateErr
		}
		return s.Repository.UpdateDelivery(ctx, deliveryID, status, receipt, detail, nextAttempt)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type deliveryPersistenceContextObservation struct {
	hasDeadline bool
	deadline    time.Time
	ctxErr      error
}

type deadlineProbeDeliveryStore struct {
	Repository
	observed chan deliveryPersistenceContextObservation
}

func (s *deadlineProbeDeliveryStore) UpdateDelivery(ctx context.Context, _ int64, _ string, _ channel.Receipt, _ string, _ time.Time) error {
	deadline, ok := ctx.Deadline()
	s.observed <- deliveryPersistenceContextObservation{hasDeadline: ok, deadline: deadline, ctxErr: ctx.Err()}
	return context.DeadlineExceeded
}

func pendingRuntimeDelivery(t *testing.T) (*sqlite.Store, int64) {
	t.Helper()
	store := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, store, 1, 1, "hello")
	testkit.FinishEvent(t, store, event, personal.TurnOutput{Text: "reply"}, []string{"reply"})
	return store, event.ID
}

func runnableRuntime(store Repository) *Runtime {
	runtime := testRuntime(store)
	runtime.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		return personal.TurnOutput{Text: "unused"}, nil
	}
	runtime.Execute = func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
		return personal.StageResult{}, nil
	}
	runtime.TaskConcurrency = 1
	runtime.idleDelay = time.Millisecond
	return runtime
}

func TestRuntimeDeliverPersistsSendOutcomeAfterContextCancellation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		attempts int
		reason   string
		sendErr  error
	}{
		{name: "sent", status: "sent", attempts: 1},
		{name: "permanent failure", status: "failed", attempts: 1, reason: "bad recipient", sendErr: &channel.SendError{Reason: "bad recipient", Permanent: true}},
		{name: "transient failure", status: "pending", attempts: 1, reason: "rate limited", sendErr: &channel.SendError{Reason: "rate limited", RetryAfter: time.Minute}},
		{name: "waiting", status: "pending", attempts: 0, reason: "waiting for login", sendErr: &channel.SendError{Reason: "waiting for login", Waiting: true}},
		{name: "unknown cancellation", status: "uncertain", attempts: 1, reason: "send outcome unknown", sendErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store, eventID := pendingRuntimeDelivery(t)
			runtime := testRuntime(store)
			receipt := channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner", MessageID: "remote-123"}
			runtime.Send = func(context.Context, channel.Content) (channel.Receipt, error) {
				cancel()
				result := receipt
				if tc.sendErr != nil {
					result.MessageID = ""
				}
				return result, tc.sendErr
			}
			if worked, err := runtime.deliver(ctx); err != nil || !worked {
				t.Fatalf("deliver = (%t, %v), want persisted send result", worked, err)
			}
			delivery := onlyDelivery(t, store, eventID)
			if delivery.Status != tc.status || delivery.Attempts != tc.attempts || delivery.Detail != tc.reason {
				t.Fatalf("delivery = %+v, want status=%q attempts=%d reason=%q", delivery, tc.status, tc.attempts, tc.reason)
			}
			if tc.status == "sent" {
				if delivery.Receipt == nil || delivery.Receipt.MessageID != receipt.MessageID {
					t.Fatalf("known success receipt was lost: %+v", delivery.Receipt)
				}
			}
		})
	}
}

func TestRuntimeRunWaitsForDeliveryPersistenceAfterCancellation(t *testing.T) {
	store, eventID := pendingRuntimeDelivery(t)
	controlled := &controlledDeliveryStore{Repository: store, entered: make(chan struct{}), release: make(chan struct{})}
	runtime := runnableRuntime(controlled)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receipt := channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner", MessageID: "sent-before-shutdown"}
	runtime.Send = func(context.Context, channel.Content) (channel.Receipt, error) {
		cancel()
		return receipt, nil
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-controlled.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateDelivery was not reached")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before receipt persistence was released: %v", err)
	default:
	}
	close(controlled.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after successful receipt persistence = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after receipt persistence")
	}
	delivery := onlyDelivery(t, store, eventID)
	if delivery.Status != "sent" || delivery.Receipt == nil || delivery.Receipt.MessageID != receipt.MessageID {
		t.Fatalf("shutdown lost remote receipt: %+v", delivery)
	}
}

func TestRuntimeRunReturnsAndJoinsDeliveryPersistenceFailureDuringShutdown(t *testing.T) {
	store, _ := pendingRuntimeDelivery(t)
	databaseErr := errors.New("injected UpdateDelivery failure")
	controlled := &controlledDeliveryStore{Repository: store, entered: make(chan struct{}), release: make(chan struct{}), updateErr: databaseErr}
	runtime := runnableRuntime(controlled)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollErr := errors.New("injected poll worker failure")
	pollReturned := make(chan struct{})
	runtime.Poll = func(context.Context) error {
		select {
		case <-controlled.entered:
			cancel()
			close(pollReturned)
			return pollErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	runtime.Send = func(context.Context, channel.Content) (channel.Receipt, error) {
		return channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner", MessageID: "accepted"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-pollReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("poll worker did not fail")
	}
	if ctx.Err() == nil {
		t.Fatal("parent context was not cancelled before releasing persistence")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before failed persistence completed: %v", err)
	default:
	}
	close(controlled.release)
	select {
	case err := <-done:
		if !errors.Is(err, databaseErr) || !errors.Is(err, pollErr) {
			t.Fatalf("Run error = %v, want both persistence and poll failures", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after failed persistence")
	}
}

func TestRuntimeRunTreatsOrdinaryParentCancellationAsCleanShutdown(t *testing.T) {
	store := testkit.EmptyStore(t, ":memory:", time.Minute)
	runtime := runnableRuntime(store)
	ready := make(chan struct{})
	runtime.Ready = func(context.Context) error {
		select {
		case <-ready:
		default:
			close(ready)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ordinary shutdown returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime did not stop after parent cancellation")
	}
}

func TestRuntimeDeliverCleanupDeadlineIsDetachedAndPropagated(t *testing.T) {
	store, eventID := pendingRuntimeDelivery(t)
	probe := &deadlineProbeDeliveryStore{Repository: store, observed: make(chan deliveryPersistenceContextObservation, 1)}
	runtime := testRuntime(probe)
	ctx, cancel := context.WithCancel(context.Background())
	runtime.Send = func(context.Context, channel.Content) (channel.Receipt, error) {
		cancel()
		return channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner", MessageID: "accepted"}, nil
	}
	worked, err := runtime.deliver(ctx)
	if !worked || !errors.Is(err, context.DeadlineExceeded) || !isDeliveryPersistenceError(err) {
		t.Fatalf("cleanup timeout persistence = (%t, %v), want wrapped DeadlineExceeded", worked, err)
	}
	observation := <-probe.observed
	remaining := time.Until(observation.deadline)
	if !observation.hasDeadline || remaining <= 0 || remaining > deliveryPersistenceTimeout || observation.ctxErr != nil {
		t.Fatalf("persistence context = %+v remaining=%s, want detached deadline within %s", observation, remaining, deliveryPersistenceTimeout)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("parent context error = %v, want canceled", ctx.Err())
	}
	if delivery := onlyDelivery(t, store, eventID); delivery.Status != "sending" {
		t.Fatalf("failed persistence unexpectedly changed delivery: %+v", delivery)
	}
}

func TestRuntimeRunDoesNotSwallowJoinedFailureAfterParentCancellation(t *testing.T) {
	store := testkit.EmptyStore(t, ":memory:", time.Minute)
	runtime := runnableRuntime(store)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	realErr := errors.New("worker database failure")
	runtime.Poll = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return errors.Join(ctx.Err(), realErr)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("poll worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, realErr) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want joined cancellation and real failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime did not stop after parent cancellation")
	}
}
