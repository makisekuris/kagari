package service

import (
	"context"
	"strings"
	"testing"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

func deliveryFactsForEvent(t *testing.T, store *sqlite.Store, eventID int64) ([]personal.DeliveryIntent, []sqlite.DeliveryRecord) {
	t.Helper()
	snapshot, err := store.Diagnostic(context.Background(), eventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var intents []personal.DeliveryIntent
	var items []sqlite.DeliveryRecord
	for _, intent := range snapshot.DeliveryIntents {
		if intent.EventID == eventID {
			intents = append(intents, intent)
		}
	}
	for _, item := range snapshot.Deliveries {
		if item.EventID == eventID {
			items = append(items, item)
		}
	}
	return intents, items
}

func TestRuntimeSuppressesCommittedDeliveryBeforeOutbox(t *testing.T) {
	for _, window := range []string{"before_presentation", "during_presentation"} {
		t.Run(window, func(t *testing.T) {
			ctx := context.Background()
			store, task, _, callback := newRunningTaskCallback(t)
			file, err := store.SaveAttachment(ctx, "report.txt", "text/plain", strings.NewReader("report"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskSettlement{
				Status: personal.TaskCompleted, Summary: "completed research",
				Delivery: personal.DeliveryDraft{Text: "completed report", Attachments: []string{file.ID}},
			}); err != nil {
				t.Fatal(err)
			}
			stop := func() {
				user := testkit.OpenUserEvent(t, store, 910, 910, "不要再发这项任务的结果")
				if _, err := store.ControlDelivery(ctx, user.ID, "stop", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err != nil {
					t.Fatal(err)
				}
				testkit.FinishEvent(t, store, user, personal.TurnOutput{Silent: true}, nil)
			}
			if window == "before_presentation" {
				stop()
			}
			// Reclaim the original callback as a real restart would.
			if err := store.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			runtime := testRuntime(store)
			runtime.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
				t.Fatal("a suppressed committed delivery re-entered the decision model")
				return personal.TurnOutput{}, nil
			}
			presentations := 0
			runtime.PresentReply = func(_ context.Context, _ personal.Event, draft string) (string, error) {
				presentations++
				if window == "before_presentation" {
					t.Fatal("already suppressed content entered the presentation model")
				}
				stop()
				return draft, nil
			}
			if worked, err := runtime.turn(ctx); !worked || err != nil {
				t.Fatalf("suppressed recovery = %t, %v", worked, err)
			}
			if window == "during_presentation" && presentations != 1 {
				t.Fatalf("presentations = %d", presentations)
			}
			intents, items := deliveryFactsForEvent(t, store, callback.ID)
			if len(intents) != 1 || len(items) != 0 {
				t.Fatalf("suppressed delivery facts: intents=%+v, items=%+v", intents, items)
			}
			intent := intents[0]
			if intent.State != personal.DeliveryIntentSuppressed || intent.Detail == "" || len(intent.Draft.Attachments) != 1 || intent.Draft.Attachments[0] != file.ID {
				t.Fatalf("suppression lost its reason or original selection: %+v", intent)
			}
			if err := store.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if worked, err := runtime.turn(ctx); worked || err != nil {
				t.Fatalf("suppressed event did not finish: %t, %v", worked, err)
			}
		})
	}
}

func TestRuntimeStopDuringSendPreservesReceiptAndSuppressesRemainingFile(t *testing.T) {
	ctx := context.Background()
	store, task, _, callback := newRunningTaskCallback(t)
	file, err := store.SaveAttachment(ctx, "report.txt", "text/plain", strings.NewReader("report"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskSettlement{
		Status: personal.TaskCompleted, Summary: "research complete",
		Delivery: personal.DeliveryDraft{Text: "report ready", Attachments: []string{file.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, callback, personal.TurnOutput{Silent: true}, nil)
	runtime := testRuntime(store)
	receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "accepted-before-stop"}
	runtime.Send = func(_ context.Context, outgoing channel.Content) (channel.Receipt, error) {
		if outgoing.AttachmentID != "" {
			t.Fatal("unexpected first send was a file")
		}
		user := testkit.OpenUserEvent(t, store, 911, 911, "不要再发送结果")
		if _, err := store.ControlDelivery(ctx, user.ID, "stop", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err != nil {
			t.Fatal(err)
		}
		testkit.FinishEvent(t, store, user, personal.TurnOutput{Silent: true}, nil)
		return receipt, nil
	}
	if worked, err := runtime.deliver(ctx); !worked || err != nil {
		t.Fatalf("delivery = %t, %v", worked, err)
	}
	_, items := deliveryFactsForEvent(t, store, callback.ID)
	if len(items) != 2 {
		t.Fatalf("delivery facts = %+v", items)
	}
	if items[0].Status != "sent" || items[0].Receipt == nil || *items[0].Receipt != receipt || items[1].Status != "suppressed" || items[1].Detail == "" {
		t.Fatalf("stop changed the known receipt or left the file sendable: %+v", items)
	}
}

func TestRuntimeCommittedControlKeepsOtherRequiredFeedback(t *testing.T) {
	ctx := context.Background()
	store, task, _, callback := newRunningTaskCallback(t)
	// Finish the callback so this fixture only exercises the following user turn.
	if _, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskSettlement{
		Status: personal.TaskWaitingInput, Summary: "stage saved", Delivery: personal.DeliveryDraft{Text: "stage result"},
	}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, callback, personal.TurnOutput{Silent: true}, nil)
	user := testkit.OpenUserEvent(t, store, 912, 912, "停止投递，并更新另一项任务")
	runtime := testRuntime(&callbackRuntimeStore{Repository: store, firstEvent: &user})
	feedback := "另一项任务未能更新，请确认需要修改的目标。"
	runtime.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if _, err := store.ControlDelivery(ctx, input.Event.ID, "stop", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err != nil {
			return personal.TurnOutput{}, err
		}
		return personal.TurnOutput{Text: "uncommitted final model output", TaskIDs: []int64{task.ID}, RequiredFeedback: feedback}, nil
	}
	runtime.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		t.Fatal("fixed control feedback was reinterpreted by the presenter")
		return "", nil
	}
	if worked, err := runtime.turn(ctx); !worked || err != nil {
		t.Fatalf("mixed control turn = %t, %v", worked, err)
	}
	intents, items := deliveryFactsForEvent(t, store, user.ID)
	if len(intents) != 2 {
		t.Fatalf("mixed control intents = %+v", intents)
	}
	found := false
	for _, item := range items {
		if item.Text == feedback {
			found = true
		}
		if item.Text == "uncommitted final model output" {
			t.Fatal("model output replaced committed control feedback")
		}
	}
	if !found {
		t.Fatalf("required feedback was hidden by the control: %+v", items)
	}
}
