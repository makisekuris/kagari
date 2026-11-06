package personal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestRuntimeUnavailableStageAttachmentFailsAndContinuesWithNextTask(t *testing.T) {
	for _, includeResultAttachment := range []bool{false, true} {
		name := "progress_evidence"
		if includeResultAttachment {
			name = "result_attachment"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			attachment, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("evidence"))
			if err != nil {
				t.Fatal(err)
			}
			firstEvent := openUserEvent(t, s, 1, 1, "Create the first task")
			firstTask, firstRun, err := s.CreateTask(ctx, firstEvent.ID, "first", "first", TaskInput{
				Goal: "Use and report the attachment", Attachments: []channel.Attachment{attachment},
			})
			if err != nil {
				t.Fatal(err)
			}
			firstTask, err = s.AddTaskInputs(ctx, firstEvent.ID, "first-input", firstTask.ID, firstTask.Revision, []int64{firstEvent.ID})
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, firstEvent, TurnOutput{}, nil)

			secondEvent := openUserEvent(t, s, 2, 2, "Create the next task")
			secondTask, secondRun := createTask(t, s, secondEvent, "second", "second", "Continue task processing", "")
			finishEvent(t, s, secondEvent, TurnOutput{}, nil)

			runtime := testRuntime(s)
			runtime.Execute = func(_ context.Context, run Run, _ func(string) error) (StageResult, error) {
				if run.ID == firstRun.ID {
					if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
						return StageResult{}, err
					}
					result := StageResult{
						Summary: "partial stage summary",
						Files:   []string{"report.txt"},
						Progress: []ProgressUpdate{{ID: "attachment-read", Description: "Read the source", Status: "reported",
							Evidence: []EvidenceRef{{Kind: "attachment", ID: attachment.ID}}}},
						AppliedInputIDs: []int64{firstEvent.ID},
					}
					if includeResultAttachment {
						result.Attachments = []channel.Attachment{attachment}
					}
					return result, nil
				}
				if run.ID == secondRun.ID {
					return StageResult{Summary: "next task ran"}, nil
				}
				t.Fatalf("unexpected run %d", run.ID)
				return StageResult{}, nil
			}

			worked, err := runtime.task(ctx)
			if err != nil || !worked {
				t.Fatalf("first task execution = (%t, %v)", worked, err)
			}
			failedRun, err := s.Run(ctx, firstRun.ID)
			if err != nil || failedRun.Status != RunFailed || !strings.Contains(failedRun.Error, "attachment became unavailable") || !strings.Contains(failedRun.Error, "progress and task input reports were not applied") {
				t.Fatalf("unavailable attachment result was not recorded as a clear failure: %+v %v", failedRun, err)
			}
			var failedResult StageResult
			if err := json.Unmarshal([]byte(failedRun.Result), &failedResult); err != nil {
				t.Fatal(err)
			}
			if failedResult.Summary != "partial stage summary" || len(failedResult.Files) != 1 || len(failedResult.Attachments) != 0 || len(failedResult.Progress) != 0 || len(failedResult.AppliedInputIDs) != 0 {
				t.Fatalf("failure result retained unapplied data or lost partial output: %+v", failedResult)
			}
			var inputStatus string
			if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, firstTask.ID, firstEvent.ID).Scan(&inputStatus); err != nil || inputStatus != "pending" {
				t.Fatalf("failed stage input status=%q err=%v, want pending", inputStatus, err)
			}
			var progressCount int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_progress WHERE task_id=?`, firstTask.ID).Scan(&progressCount); err != nil || progressCount != 0 {
				t.Fatalf("failed stage progress count=%d err=%v, want 0", progressCount, err)
			}

			worked, err = runtime.task(ctx)
			if err != nil || !worked {
				t.Fatalf("next task execution = (%t, %v)", worked, err)
			}
			completedRun, err := s.Run(ctx, secondRun.ID)
			if err != nil || completedRun.Status != RunCompleted {
				t.Fatalf("next task did not run after attachment failure: %+v %v", completedRun, err)
			}
			completedTask, err := s.Task(ctx, secondTask.ID)
			if err != nil || completedTask.Status != TaskAwaitingReview {
				t.Fatalf("next task state=%+v err=%v", completedTask, err)
			}
		})
	}
}

func TestRuntimeIgnoresLateCancelledRunBeforeValidatingAttachment(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	attachment, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	event := openUserEvent(t, s, 1, 1, "Create a task")
	task, run, err := s.CreateTask(ctx, event.ID, "cancelled", "cancelled", TaskInput{
		Goal: "Read the attachment", Attachments: []channel.Attachment{attachment},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)

	runtime := testRuntime(s)
	runtime.Execute = func(_ context.Context, current Run, _ func(string) error) (StageResult, error) {
		cancelEvent := openUserEvent(t, s, 2, 2, "Cancel the task")
		if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, current.Revision); err != nil {
			return StageResult{}, err
		}
		if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
			return StageResult{}, err
		}
		finishEvent(t, s, cancelEvent, TurnOutput{}, nil)
		return StageResult{Summary: "late result", Attachments: []channel.Attachment{attachment}}, nil
	}

	worked, err := runtime.task(ctx)
	if err != nil || !worked {
		t.Fatalf("cancelled task execution = (%t, %v)", worked, err)
	}
	savedRun, err := s.Run(ctx, run.ID)
	if err != nil || savedRun.Status != RunCancelled {
		t.Fatalf("late result changed cancelled run: %+v %v", savedRun, err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Attachments: []channel.Attachment{attachment}}, ""); err != nil {
		t.Fatalf("terminal run validated a discarded attachment: %v", err)
	}
}
