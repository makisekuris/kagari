package assistant

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

type settlementCaptureRepository struct {
	Repository
	task              personal.Task
	files             map[string]channel.Attachment
	settlement        *personal.TaskSettlement
	attachmentLookups []string
	continueErr       error
}

func (r *settlementCaptureRepository) SaveAttachment(_ context.Context, name, mime string, contents io.Reader) (channel.Attachment, error) {
	body, err := io.ReadAll(contents)
	if err != nil {
		return channel.Attachment{}, err
	}
	id := fmt.Sprintf("created-%d", len(r.files)+1)
	file := channel.Attachment{ID: id, Name: personal.SafeAttachmentName(name), MIME: mime, Size: int64(len(body))}
	if r.files == nil {
		r.files = make(map[string]channel.Attachment)
	}
	r.files[id] = file
	return file, nil
}

func (r *settlementCaptureRepository) Attachment(_ context.Context, id string) (channel.Attachment, error) {
	r.attachmentLookups = append(r.attachmentLookups, id)
	file, ok := r.files[id]
	if !ok {
		return channel.Attachment{}, fmt.Errorf("attachment %q is unavailable", id)
	}
	return file, nil
}

func (r *settlementCaptureRepository) OpenAttachment(_ context.Context, id string) (io.ReadCloser, error) {
	if _, ok := r.files[id]; !ok {
		return nil, fmt.Errorf("attachment %q is unavailable", id)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (r *settlementCaptureRepository) ContinueTask(ctx context.Context, eventID int64, requestKey string, taskID, revision int64, step string, extend bool) (personal.Task, personal.Run, error) {
	if r.continueErr != nil {
		return personal.Task{}, personal.Run{}, r.continueErr
	}
	return r.Repository.ContinueTask(ctx, eventID, requestKey, taskID, revision, step, extend)
}

func (r *settlementCaptureRepository) SettleTask(_ context.Context, _, taskID, revision int64, settlement personal.TaskSettlement) (personal.Task, error) {
	if r.settlement != nil {
		return personal.Task{}, errors.New("settlement called more than once")
	}
	copy := settlement
	copy.Delivery.Attachments = append([]string{}, settlement.Delivery.Attachments...)
	r.settlement = &copy
	r.task.ID, r.task.Revision = taskID, revision
	r.task.Status, r.task.Summary = settlement.Status, settlement.Summary
	return r.task, nil
}

func newSettlementCaptureRepository(task personal.Task) *settlementCaptureRepository {
	return &settlementCaptureRepository{Repository: newToolRepo(), task: task, files: make(map[string]channel.Attachment)}
}

func settlementTurnInput(task personal.Task, run personal.Run) personal.TurnInput {
	return personal.TurnInput{
		Event:      personal.Event{ID: 201, Kind: personal.EventTask, TaskID: task.ID, RunID: run.ID, Text: "stage result"},
		Tasks:      []personal.Task{task},
		CurrentRun: &run,
	}
}

func TestSettleTaskRequiresExplicitAttachmentSelectionAndSupportsFileOnly(t *testing.T) {
	task := personal.Task{ID: 7, Revision: 2, Status: personal.TaskAwaitingReview}
	repo := newSettlementCaptureRepository(task)
	repo.files["file-1"] = channel.Attachment{ID: "file-1", Name: "one.txt", MIME: "text/plain"}
	repo.files["file-2"] = channel.Attachment{ID: "file-2", Name: "two.txt", MIME: "text/plain"}
	event := personal.Event{ID: 201, Kind: personal.EventTask, TaskID: task.ID}
	owner, tools := newTaskActionTools(t, repo, event, personal.TurnInput{Tasks: []personal.Task{task}})

	missingSelection := invokeActionTool(t, tools, "settle_task", settleTaskArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskCompleted, Summary: "Stored task result",
		Delivery: personal.DeliveryDraft{Text: "Ready for you."},
	})
	if missingSelection.OK || len(repo.attachmentLookups) != 0 || repo.settlement != nil {
		t.Fatalf("settlement without explicit attachment array = %+v; lookups=%v settlement=%+v", missingSelection, repo.attachmentLookups, repo.settlement)
	}

	missingFile := invokeActionTool(t, tools, "settle_task", settleTaskArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskCompleted, Summary: "Stored task result",
		Delivery: personal.DeliveryDraft{Attachments: []string{"missing-file"}},
	})
	if missingFile.OK || repo.settlement != nil {
		t.Fatalf("settlement with unavailable file = %+v; settlement=%+v", missingFile, repo.settlement)
	}

	fileOnly := invokeActionTool(t, tools, "settle_task", settleTaskArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskCompleted, Summary: "The complete report is saved in the task result.",
		Delivery: personal.DeliveryDraft{Text: "", Attachments: []string{"file-2", "file-1"}},
	})
	if !fileOnly.OK || repo.settlement == nil {
		t.Fatalf("valid file-only settlement = %+v; settlement=%+v", fileOnly, repo.settlement)
	}
	if repo.settlement.Summary != "The complete report is saved in the task result." || repo.settlement.Delivery.Text != "" ||
		!sameStringSlice(repo.settlement.Delivery.Attachments, []string{"file-2", "file-1"}) {
		t.Fatalf("saved settlement did not preserve separate summary and selected order: %+v", repo.settlement)
	}
	if owner.settledDelivery == nil || owner.settledDelivery.Text != "" || !sameStringSlice(owner.settledDelivery.Attachments, []string{"file-2", "file-1"}) {
		t.Fatalf("assistant did not retain fixed file-only intent: %+v", owner.settledDelivery)
	}
}

func TestThinkSettlementKeepsOnlyChosenFilesWhenFinalModelSelectsAnother(t *testing.T) {
	task := personal.Task{ID: 7, Title: "Comparison", Goal: "Compare the options", Revision: 2, Status: personal.TaskAwaitingReview}
	run := personal.Run{ID: 31, TaskID: task.ID, Revision: task.Revision, Kind: personal.RunTask, Status: personal.RunCompleted, Result: `{"summary":"Evidence was compared."}`}
	repo := newSettlementCaptureRepository(task)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("write_attachment", map[string]string{"name": "first.txt", "text": "first"}, "write-first"),
		testkit.ResponseTool("write_attachment", map[string]string{"name": "second.txt", "text": "second"}, "write-second"),
		testkit.ResponseTool("settle_task", settleTaskArgs{
			TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskCompleted, Summary: "Both options were compared; option two is suitable.",
			Delivery: personal.DeliveryDraft{Text: "Option two is suitable. The comparison report is attached.", Attachments: []string{"created-2"}},
		}, "settle-with-selected-file"),
		testkit.ResponseText(`{"text":"The report is ready.","reply_kind":"content","attachments":["created-1"]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), settlementTurnInput(task, run))
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.files) != 2 || repo.settlement == nil || !sameStringSlice(repo.settlement.Delivery.Attachments, []string{"created-2"}) {
		t.Fatalf("settlement did not save only the explicitly chosen file: files=%v settlement=%+v", repo.files, repo.settlement)
	}
	if output.Text != repo.settlement.Delivery.Text || output.Silent || !sameStringSlice(output.Attachments, []string{"created-2"}) {
		t.Fatalf("final model response changed saved delivery: output=%+v intent=%+v", output, repo.settlement.Delivery)
	}
	if len(fixture.Requests) != 4 {
		t.Fatalf("post-settlement response triggered formatting or another model call: %d requests", len(fixture.Requests))
	}
	if !sameStringSlice(repo.attachmentLookups, []string{"created-2"}) {
		t.Fatalf("assistant validated files beyond the stored selection: %v", repo.attachmentLookups)
	}
}

func TestThinkSettlementPersistsUnresolvedContinuationFailure(t *testing.T) {
	task := personal.Task{ID: 7, Title: "Comparison", Goal: "Compare the options", Revision: 2, Status: personal.TaskAwaitingReview}
	run := personal.Run{ID: 31, TaskID: task.ID, Revision: task.Revision, Kind: personal.RunTask, Status: personal.RunCompleted}
	repo := newSettlementCaptureRepository(task)
	repo.continueErr = errors.New("temporary continuation failure")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("continue_task", continueTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision, Step: "Check one remaining source"}, "continue-fails"),
		testkit.ResponseTool("settle_task", settleTaskArgs{
			TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskFailed, Summary: "The requested continuation could not be completed.",
			Delivery: personal.DeliveryDraft{Text: "The requested continuation could not be completed.", Attachments: []string{}},
		}, "settle-with-failure"),
		testkit.ResponseText(`{"text":"This final response cannot replace the saved delivery.","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), settlementTurnInput(task, run))
	if err != nil {
		t.Fatal(err)
	}
	if repo.settlement == nil {
		t.Fatal("task was not settled")
	}
	wantNotice := "「Comparison」未能继续执行。这部分尚未完成，可以让我重试。"
	if !strings.Contains(repo.settlement.Delivery.Text, wantNotice) {
		t.Fatalf("settlement did not persist unresolved action notice: %+v", repo.settlement.Delivery)
	}
	if output.Text != repo.settlement.Delivery.Text || !strings.Contains(output.Text, wantNotice) {
		t.Fatalf("output did not reuse committed delivery: output=%+v intent=%+v", output, repo.settlement.Delivery)
	}
}

func TestThinkSettlementIgnoresUnavailableFinalFileAndRecoversModelFailure(t *testing.T) {
	t.Run("unavailable final selection is ignored", func(t *testing.T) {
		task := personal.Task{ID: 7, Title: "Report", Goal: "Write report", Revision: 2, Status: personal.TaskAwaitingReview}
		run := personal.Run{ID: 31, TaskID: task.ID, Revision: task.Revision, Kind: personal.RunTask, Status: personal.RunCompleted}
		repo := newSettlementCaptureRepository(task)
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("write_attachment", map[string]string{"name": "report.txt", "text": "report"}, "write-report"),
			testkit.ResponseTool("settle_task", settleTaskArgs{
				TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskCompleted, Summary: "The report is complete.",
				Delivery: personal.DeliveryDraft{Text: "The report is ready.", Attachments: []string{"created-1"}},
			}, "settle-report"),
			testkit.ResponseText(`{"text":"Done.","reply_kind":"content","attachments":["unavailable-final-file"]}`),
		}}
		output, err := newMainAgent(t, fixture, repo).Think(context.Background(), settlementTurnInput(task, run))
		if err != nil || output.Text != "The report is ready." || !sameStringSlice(output.Attachments, []string{"created-1"}) {
			t.Fatalf("final unavailable selection changed committed intent: output=%+v error=%v", output, err)
		}
		if !sameStringSlice(repo.attachmentLookups, []string{"created-1"}) {
			t.Fatalf("assistant validated the final model's unselected file: %v", repo.attachmentLookups)
		}
	})

	t.Run("format error after settlement uses committed delivery", func(t *testing.T) {
		task := personal.Task{ID: 7, Title: "Report", Goal: "Write report", Revision: 2, Status: personal.TaskAwaitingReview}
		run := personal.Run{ID: 31, TaskID: task.ID, Revision: task.Revision, Kind: personal.RunTask, Status: personal.RunCompleted}
		repo := newSettlementCaptureRepository(task)
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("settle_task", settleTaskArgs{
				TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskFailed, Summary: "The export failed after the review was completed.",
				Delivery: personal.DeliveryDraft{Text: "The export failed, but the review is available below.", Attachments: []string{}},
			}, "settle-before-format-error"),
			testkit.ResponseText("not valid reply JSON"),
		}}
		output, err := newMainAgent(t, fixture, repo).Think(context.Background(), settlementTurnInput(task, run))
		if err != nil || output.Text != "The export failed, but the review is available below." || output.Silent || len(output.Attachments) != 0 {
			t.Fatalf("format error lost committed delivery: output=%+v error=%v", output, err)
		}
		if len(fixture.Requests) != 2 {
			t.Fatalf("format error after settlement triggered another model call: %d requests", len(fixture.Requests))
		}
	})

	t.Run("model failure after settlement uses committed file-only draft", func(t *testing.T) {
		task := personal.Task{ID: 7, Title: "Report", Goal: "Write report", Revision: 2, Status: personal.TaskAwaitingReview}
		run := personal.Run{ID: 31, TaskID: task.ID, Revision: task.Revision, Kind: personal.RunTask, Status: personal.RunCompleted}
		repo := newSettlementCaptureRepository(task)
		incomplete := testkit.ResponseText(`{"text":"partial","reply_kind":"content","attachments":[]}`)
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("write_attachment", map[string]string{"name": "report.txt", "text": "report"}, "write-report"),
			testkit.ResponseTool("settle_task", settleTaskArgs{
				TaskID: task.ID, ExpectedRevision: task.Revision, Status: personal.TaskCompleted, Summary: "The report is complete and retained as the task result.",
				Delivery: personal.DeliveryDraft{Text: "", Attachments: []string{"created-1"}},
			}, "settle-file-only"),
			{Status: "incomplete", Output: incomplete.Output},
		}}
		output, err := newMainAgent(t, fixture, repo).Think(context.Background(), settlementTurnInput(task, run))
		if err != nil || output.Text != "" || output.Silent || !sameStringSlice(output.Attachments, []string{"created-1"}) {
			t.Fatalf("model failure lost committed file-only delivery: output=%+v error=%v", output, err)
		}
		if repo.settlement == nil || strings.TrimSpace(repo.settlement.Summary) == "" || repo.settlement.Delivery.Text != "" ||
			!sameStringSlice(repo.settlement.Delivery.Attachments, []string{"created-1"}) {
			t.Fatalf("file-only settlement lost separate task summary: %+v", repo.settlement)
		}
		if len(fixture.Requests) != 3 {
			t.Fatalf("model failure after settlement triggered formatter: %d requests", len(fixture.Requests))
		}
	})
}
