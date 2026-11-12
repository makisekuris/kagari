package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"kagari/internal/personal"
)

func TestRuntimeSettlementDeliverySurvivesInterruption(t *testing.T) {
	for _, status := range []personal.TaskStatus{personal.TaskCompleted, personal.TaskFailed, personal.TaskWaitingInput} {
		for _, cut := range []string{"after_settlement", "during_presentation"} {
			t.Run(string(status)+"/"+cut, func(t *testing.T) {
				ctx := context.Background()
				store, task, run, callback := newRunningTaskCallback(t)
				old, err := store.SaveAttachment(ctx, "report.txt", "text/plain", strings.NewReader("version one"))
				if err != nil {
					t.Fatal(err)
				}
				latest, err := store.SaveAttachment(ctx, "report.txt", "text/plain", strings.NewReader("version two"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.SaveAttachment(ctx, "notes.txt", "text/plain", strings.NewReader("not selected")); err != nil {
					t.Fatal(err)
				}
				selection := []string{latest.ID, old.ID}
				settlement := personal.TaskSettlement{Status: status, Summary: "saved task evidence", Delivery: personal.DeliveryDraft{Text: "the selected reports", Attachments: selection}}
				if _, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, settlement); err != nil {
					t.Fatal(err)
				}
				before, err := store.EventDeliveries(ctx, callback.ID)
				if err != nil || len(before) != 1 {
					t.Fatalf("saved intent = %+v, %v", before, err)
				}
				if err := store.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				runtime := testRuntime(store)
				runtime.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
					t.Fatal("recovery called the model instead of consuming the saved settlement")
					return personal.TurnOutput{}, nil
				}
				if cut == "during_presentation" {
					interrupted, cancel := context.WithCancel(ctx)
					runtime.PresentReply = func(context.Context, personal.Event, string) (string, error) {
						cancel()
						return "", context.Canceled
					}
					if worked, err := runtime.turn(interrupted); !worked || !errors.Is(err, context.Canceled) {
						t.Fatalf("interrupted presentation = %t, %v", worked, err)
					}
					cancel()
					pending, err := store.EventDeliveries(ctx, callback.ID)
					if err != nil || len(pending) != 1 || pending[0].ID != before[0].ID || !reflect.DeepEqual(pending[0].Draft.Attachments, selection) {
						t.Fatalf("interruption changed intent: %+v, %v", pending, err)
					}
					if err := store.Recover(ctx); err != nil {
						t.Fatal(err)
					}
				}
				runtime.PresentReply = func(_ context.Context, _ personal.Event, draft string) (string, error) {
					return "presented: " + draft, nil
				}
				if worked, err := runtime.turn(ctx); !worked || err != nil {
					t.Fatalf("recovery = %t, %v", worked, err)
				}
				snapshot, err := store.Diagnostic(ctx, callback.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.DeliveryIntents) != 1 || snapshot.DeliveryIntents[0].ID != before[0].ID || !reflect.DeepEqual(snapshot.DeliveryIntents[0].Draft.Attachments, selection) {
					t.Fatalf("saved identity/selection changed: %+v", snapshot.DeliveryIntents)
				}
				intent := snapshot.DeliveryIntents[0]
				if len(intent.Scopes) != 1 || intent.Scopes[0] != (personal.DeliveryScope{TaskID: task.ID, Revision: task.Revision, RunID: run.ID}) {
					t.Fatalf("wrong immutable source: %+v", intent.Scopes)
				}
				if len(snapshot.Deliveries) != 3 || snapshot.Deliveries[0].Text != "presented: the selected reports" || snapshot.Deliveries[1].AttachmentID != latest.ID || snapshot.Deliveries[2].AttachmentID != old.ID {
					t.Fatalf("wrong prepared payload: %+v", snapshot.Deliveries)
				}
				for i, item := range snapshot.Deliveries {
					if item.DeliveryID != intent.ID || item.DeliveryPart != i+1 {
						t.Fatalf("wrong delivery part: %+v", item)
					}
				}
				if err := store.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if worked, err := runtime.turn(ctx); worked || err != nil {
					t.Fatalf("committed outbox was rendered again: %t, %v", worked, err)
				}
				after, err := store.Diagnostic(ctx, callback.ID, 0)
				if err != nil || !reflect.DeepEqual(snapshot.Deliveries, after.Deliveries) {
					t.Fatalf("post-commit recovery changed outbox: %+v, %v", after.Deliveries, err)
				}
				got, err := store.Task(ctx, task.ID)
				if err != nil || got.Status != status || got.Summary != settlement.Summary {
					t.Fatalf("recovery changed settlement: %+v, %v", got, err)
				}
			})
		}
	}
}

func TestRuntimeSettlementOwnsFileOnlySelection(t *testing.T) {
	ctx := context.Background()
	store, task, _, callback := newRunningTaskCallback(t)
	selected, err := store.SaveAttachment(ctx, "chosen.txt", "text/plain", strings.NewReader("chosen"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(&callbackRuntimeStore{Repository: store, firstEvent: &callback})
	runtime.Think = func(ctx context.Context, _ personal.TurnInput) (personal.TurnOutput, error) {
		_, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskSettlement{
			Status: personal.TaskCompleted, Summary: "evidence retained without a text message",
			Delivery: personal.DeliveryDraft{Attachments: []string{selected.ID}},
		})
		if err != nil {
			return personal.TurnOutput{}, err
		}
		return personal.TurnOutput{Text: "uncommitted final text", Attachments: []string{"nonexistent-final-selection"}}, errors.New("model failed after committing settlement")
	}
	runtime.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		t.Fatal("attachment-only delivery unexpectedly generated a text message")
		return "", nil
	}
	if worked, err := runtime.turn(ctx); !worked || err != nil {
		t.Fatalf("settled turn = %t, %v", worked, err)
	}
	message, err := store.ClaimDelivery(ctx)
	if err != nil || message == nil || message.Text != "" || message.AttachmentID != selected.ID {
		t.Fatalf("file selection changed: %+v, %v", message, err)
	}
	got, err := store.Task(ctx, task.ID)
	if err != nil || got.Summary != "evidence retained without a text message" {
		t.Fatalf("file-only summary lost: %+v, %v", got, err)
	}
}

func TestRuntimeDeletedSettlementAttachmentKeepsFailureAndSelection(t *testing.T) {
	ctx := context.Background()
	store, task, _, callback := newRunningTaskCallback(t)
	selected, err := store.SaveAttachment(ctx, "chosen.txt", "text/plain", strings.NewReader("chosen"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "business complete", Delivery: personal.DeliveryDraft{Attachments: []string{selected.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAttachment(ctx, selected.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(store)
	if worked, err := runtime.turn(ctx); !worked || err != nil {
		t.Fatalf("deleted attachment recovery = %t, %v", worked, err)
	}
	snapshot, err := store.Diagnostic(ctx, callback.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.DeliveryIntents) != 1 || snapshot.DeliveryIntents[0].Detail == "" || !reflect.DeepEqual(snapshot.DeliveryIntents[0].Draft.Attachments, []string{selected.ID}) {
		t.Fatalf("unavailable selection was lost: %+v", snapshot.DeliveryIntents)
	}
	if len(snapshot.Deliveries) != 1 || snapshot.Deliveries[0].AttachmentID != "" || !strings.Contains(snapshot.Deliveries[0].Text, "附件已删除或不可用") {
		t.Fatalf("missing unavailable-file notice: %+v", snapshot.Deliveries)
	}
	got, err := store.Task(ctx, task.ID)
	if err != nil || got.Status != personal.TaskCompleted {
		t.Fatalf("delivery failure rolled back task: %+v, %v", got, err)
	}
}
