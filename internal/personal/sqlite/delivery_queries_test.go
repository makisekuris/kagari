package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"kagari/internal/personal"
)

func TestDeliveryQueriesKeepGroupsScopesAndCursor(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "create two tasks")
	a, _ := createTask(t, s, first, "a", "A", "A goal", "")
	b, _ := createTask(t, s, first, "b", "B", "B goal", "")
	finishEvent(t, s, first, personal.TurnOutput{Text: "combined", TaskIDs: []int64{a.ID, b.ID}}, nil)
	second := openUserEvent(t, s, 2, 2, "A result")
	finishEvent(t, s, second, personal.TurnOutput{Text: "A only", TaskID: a.ID}, nil)
	third := openUserEvent(t, s, 3, 3, "plain conversation")
	finishEvent(t, s, third, personal.TurnOutput{Text: "unscoped"}, nil)
	user := openUserEvent(t, s, 4, 4, "show deliveries")
	scope := personal.ReadScope{EventID: user.ID}
	page, err := s.QueryDeliveries(ctx, scope, personal.DeliveryQuery{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Draft.Text != "unscoped" || page.NextBeforeID != page.Items[0].ID {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	next, err := s.QueryDeliveries(ctx, scope, personal.DeliveryQuery{BeforeID: page.NextBeforeID, Limit: 1})
	if err != nil || len(next.Items) != 1 || next.Items[0].Draft.Text != "A only" || next.NextBeforeID == 0 {
		t.Fatalf("next page = %+v, %v", next, err)
	}
	filtered, err := s.QueryDeliveries(ctx, scope, personal.DeliveryQuery{TaskID: b.ID})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Draft.Text != "combined" || len(filtered.Items[0].Scopes) != 2 {
		t.Fatalf("multi-Task scope filter = %+v, %v", filtered, err)
	}
	stored, err := s.Delivery(ctx, scope, filtered.Items[0].ID)
	if err != nil || !reflect.DeepEqual(*stored, filtered.Items[0]) {
		t.Fatalf("specific delivery differs from query: %+v, %v", stored, err)
	}
	if _, err := s.ControlDelivery(ctx, user.ID, "stop-a", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: a.ID}); err != nil {
		t.Fatal(err)
	}
	empty, err := s.QueryDeliveries(ctx, scope, personal.DeliveryQuery{TaskID: a.ID, BeforeID: 1})
	if err != nil || len(empty.Items) != 0 || !reflect.DeepEqual(empty.StoppedTaskIDs, []int64{a.ID}) {
		t.Fatalf("empty page lost persistent gate: %+v, %v", empty, err)
	}
	if _, err := s.Delivery(ctx, scope, 999999); !errors.Is(err, personal.ErrRecordNotFound) {
		t.Fatalf("missing delivery error = %v", err)
	}
	if _, err := s.QueryDeliveries(ctx, scope, personal.DeliveryQuery{TaskID: 999999}); !errors.Is(err, personal.ErrRecordNotFound) {
		t.Fatalf("missing Task filter error = %v", err)
	}
	for _, invalid := range []personal.ReadScope{{}, {EventID: first.ID}, {RunID: 1}, {EventID: user.ID, RunID: 1}} {
		if _, err := s.Delivery(ctx, invalid, filtered.Items[0].ID); err == nil {
			t.Fatalf("invalid scope accepted: %+v", invalid)
		}
	}
	for _, invalid := range []personal.DeliveryQuery{{Limit: -1}, {Limit: personal.MaxDeliveryPageSize + 1}, {TaskID: -1}, {BeforeID: -1}} {
		if _, err := s.QueryDeliveries(ctx, scope, invalid); err == nil {
			t.Fatalf("invalid query accepted: %+v", invalid)
		}
	}
}

func TestDeliveryQueriesRejectCallbackAuthority(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, testTaskSettlement(personal.TaskWaitingInput, "saved result")); err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(intents) != 1 {
		t.Fatalf("saved intent = %+v, %v", intents, err)
	}
	scope := personal.ReadScope{EventID: callback.ID}
	if _, err := s.Delivery(ctx, scope, intents[0].ID); err == nil {
		t.Fatal("callback acquired direct-user delivery query authority")
	}
	if _, err := s.QueryDeliveries(ctx, scope, personal.DeliveryQuery{TaskID: task.ID}); err == nil {
		t.Fatal("callback acquired direct-user delivery list authority")
	}
}
