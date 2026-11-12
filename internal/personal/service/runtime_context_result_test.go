package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeUnavailableStageAttachmentFailsAndContinuesWithNextTask(t *testing.T) {
	for _, includeResultAttachment := range []bool{false, true} {
		name := "progress_evidence"
		if includeResultAttachment {
			name = "result_attachment"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			attachment, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("evidence"))
			if err != nil {
				t.Fatal(err)
			}
			firstEvent := testkit.OpenUserEvent(t, s, 1, 1, "Create the first task")
			firstTask, firstRun, err := s.CreateTask(ctx, firstEvent.ID, "first", "first", personal.TaskInput{
				Goal: "Use and report the attachment", Attachments: []channel.Attachment{attachment},
			})
			if err != nil {
				t.Fatal(err)
			}
			firstTask, err = s.AddTaskInputs(ctx, firstEvent.ID, "first-input", firstTask.ID, firstTask.Revision, []int64{firstEvent.ID})
			if err != nil {
				t.Fatal(err)
			}
			testkit.FinishEvent(t, s, firstEvent, personal.TurnOutput{}, nil)

			secondEvent := testkit.OpenUserEvent(t, s, 2, 2, "Create the next task")
			secondTask, secondRun := testkit.CreateTask(t, s, secondEvent, "second", "second", "Continue task processing", "")
			testkit.FinishEvent(t, s, secondEvent, personal.TurnOutput{}, nil)

			runtime := testRuntime(s)
			runtime.Execute = func(_ context.Context, run personal.Run, _ func(string) error) (personal.StageResult, error) {
				if run.ID == firstRun.ID {
					if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
						return personal.StageResult{}, err
					}
					result := personal.StageResult{
						Summary: "partial stage summary",
						Files:   []string{"report.txt"},
						Progress: []personal.ProgressUpdate{{ID: "attachment-read", Description: "Read the source", Status: "reported",
							Evidence: []personal.EvidenceRef{{Kind: "attachment", ID: attachment.ID}}}},
						AppliedInputIDs: []int64{firstEvent.ID},
					}
					if includeResultAttachment {
						result.Attachments = []channel.Attachment{attachment}
					}
					return result, nil
				}
				if run.ID == secondRun.ID {
					return personal.StageResult{Summary: "next task ran"}, nil
				}
				t.Fatalf("unexpected run %d", run.ID)
				return personal.StageResult{}, nil
			}

			worked, err := runtime.task(ctx)
			if err != nil || !worked {
				t.Fatalf("first task execution = (%t, %v)", worked, err)
			}
			failedRun, err := s.Run(ctx, firstRun.ID)
			if err != nil || failedRun.Status != personal.RunFailed || !strings.Contains(failedRun.Error, "attachment became unavailable") || !strings.Contains(failedRun.Error, "progress and task input reports were not applied") {
				t.Fatalf("unavailable attachment result was not recorded as a clear failure: %+v %v", failedRun, err)
			}
			var failedResult personal.StageResult
			if err := json.Unmarshal([]byte(failedRun.Result), &failedResult); err != nil {
				t.Fatal(err)
			}
			if failedResult.Summary != "partial stage summary" || len(failedResult.Files) != 1 || len(failedResult.Attachments) != 0 || len(failedResult.Progress) != 0 || len(failedResult.AppliedInputIDs) != 0 {
				t.Fatalf("failure result retained unapplied data or lost partial output: %+v", failedResult)
			}
			pending, err := s.PendingTaskInputs(ctx, firstTask.ID)
			if err != nil || len(pending) != 1 || pending[0].ID != firstEvent.ID {
				t.Fatalf("failed stage must leave its input outstanding: %+v, %v", pending, err)
			}
			review, err := s.ClaimEvent(ctx)
			if err != nil || review == nil || review.RunID != firstRun.ID {
				t.Fatalf("failed stage review = %+v, %v", review, err)
			}
			progress, err := s.TaskProgress(ctx, personal.ReadScope{EventID: review.ID}, firstTask.ID, personal.ProgressQuery{})
			if err != nil || len(progress.Entries) != 0 {
				t.Fatalf("failed stage must not persist progress: %+v, %v", progress, err)
			}

			worked, err = runtime.task(ctx)
			if err != nil || !worked {
				t.Fatalf("next task execution = (%t, %v)", worked, err)
			}
			completedRun, err := s.Run(ctx, secondRun.ID)
			if err != nil || completedRun.Status != personal.RunCompleted {
				t.Fatalf("next task did not run after attachment failure: %+v %v", completedRun, err)
			}
			completedTask, err := s.Task(ctx, secondTask.ID)
			if err != nil || completedTask.Status != personal.TaskAwaitingReview {
				t.Fatalf("next task state=%+v err=%v", completedTask, err)
			}
		})
	}
}

func TestRuntimeIgnoresLateCancelledRunBeforeValidatingAttachment(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	attachment, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	event := testkit.OpenUserEvent(t, s, 1, 1, "Create a task")
	task, run, err := s.CreateTask(ctx, event.ID, "cancelled", "cancelled", personal.TaskInput{
		Goal: "Read the attachment", Attachments: []channel.Attachment{attachment},
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)

	runtime := testRuntime(s)
	runtime.Execute = func(_ context.Context, current personal.Run, _ func(string) error) (personal.StageResult, error) {
		cancelEvent := testkit.OpenUserEvent(t, s, 2, 2, "Cancel the task")
		if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, current.Revision); err != nil {
			return personal.StageResult{}, err
		}
		if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
			return personal.StageResult{}, err
		}
		testkit.FinishEvent(t, s, cancelEvent, personal.TurnOutput{}, nil)
		return personal.StageResult{Summary: "late result", Attachments: []channel.Attachment{attachment}}, nil
	}

	worked, err := runtime.task(ctx)
	if err != nil || !worked {
		t.Fatalf("cancelled task execution = (%t, %v)", worked, err)
	}
	savedRun, err := s.Run(ctx, run.ID)
	if err != nil || savedRun.Status != personal.RunCancelled {
		t.Fatalf("late result changed cancelled run: %+v %v", savedRun, err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Attachments: []channel.Attachment{attachment}}, ""); err != nil {
		t.Fatalf("terminal run validated a discarded attachment: %v", err)
	}
}
