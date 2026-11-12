package assistant

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

type deliveryControlCall struct {
	eventID int64
	key     string
	request personal.DeliveryControlRequest
}

type deliveryToolRepository struct {
	*toolRepo
	deliveries         map[int64]personal.DeliveryIntent
	eventDeliveries    map[int64][]personal.DeliveryIntent
	queryPage          personal.DeliveryPage
	queryErr           error
	controlErr         error
	controlDraft       *personal.DeliveryDraft
	controlCalls       []deliveryControlCall
	deliveryQueries    []int64
	queryDeliveryCalls int
	cancelCalls        int
	reviseErr          error
	nextDeliveryID     int64
	schedules          map[int64]personal.Schedule
	scheduleQueries    []int64
}

func newDeliveryToolRepository() *deliveryToolRepository {
	return &deliveryToolRepository{
		toolRepo:        newToolRepo(),
		deliveries:      make(map[int64]personal.DeliveryIntent),
		eventDeliveries: make(map[int64][]personal.DeliveryIntent),
		nextDeliveryID:  100,
		schedules:       make(map[int64]personal.Schedule),
	}
}

func (r *deliveryToolRepository) addIntent(intent personal.DeliveryIntent) {
	r.deliveries[intent.ID] = intent
	r.eventDeliveries[intent.EventID] = append(r.eventDeliveries[intent.EventID], intent)
}

func (r *deliveryToolRepository) ControlDelivery(_ context.Context, eventID int64, key string, request personal.DeliveryControlRequest) (personal.DeliveryControlResult, error) {
	r.controlCalls = append(r.controlCalls, deliveryControlCall{eventID: eventID, key: key, request: request})
	if r.controlErr != nil {
		return personal.DeliveryControlResult{}, r.controlErr
	}
	r.nextDeliveryID++
	intent := personal.DeliveryIntent{
		ID:      r.nextDeliveryID,
		EventID: eventID,
		Purpose: personal.DeliveryControl,
		State:   personal.DeliveryIntentPending,
		Draft:   personal.DeliveryDraft{Text: "Store-confirmed delivery control.", Attachments: []string{}},
	}
	if r.controlDraft != nil {
		intent.Draft = personal.DeliveryDraft{
			Text:        r.controlDraft.Text,
			Attachments: append([]string{}, r.controlDraft.Attachments...),
		}
	}
	if request.Action == personal.DeliveryResend {
		original, ok := r.deliveries[request.DeliveryID]
		if !ok {
			return personal.DeliveryControlResult{}, personal.ErrRecordNotFound
		}
		intent.Purpose = personal.DeliveryHistorical
		intent.OriginalDeliveryID = original.ID
		intent.Draft = original.Draft
		intent.Scopes = append([]personal.DeliveryScope(nil), original.Scopes...)
	} else if request.TaskID > 0 {
		revision := int64(1)
		if task, ok := r.tasks[request.TaskID]; ok {
			revision = task.Revision
		}
		intent.Scopes = []personal.DeliveryScope{{TaskID: request.TaskID, Revision: revision}}
	} else if original, ok := r.deliveries[request.DeliveryID]; ok {
		intent.Scopes = append([]personal.DeliveryScope(nil), original.Scopes...)
	}
	r.addIntent(intent)
	return personal.DeliveryControlResult{ID: intent.ID, Request: request, DeliveryID: intent.ID, CreatedAt: intent.CreatedAt}, nil
}

func (r *deliveryToolRepository) Delivery(_ context.Context, _ personal.ReadScope, id int64) (*personal.DeliveryIntent, error) {
	r.deliveryQueries = append(r.deliveryQueries, id)
	intent, ok := r.deliveries[id]
	if !ok {
		return nil, nil
	}
	copy := intent
	return &copy, nil
}

func (r *deliveryToolRepository) QueryDeliveries(_ context.Context, _ personal.ReadScope, _ personal.DeliveryQuery) (personal.DeliveryPage, error) {
	r.queryDeliveryCalls++
	return r.queryPage, r.queryErr
}

func (r *deliveryToolRepository) Schedule(_ context.Context, id int64) (*personal.Schedule, error) {
	r.scheduleQueries = append(r.scheduleQueries, id)
	schedule, ok := r.schedules[id]
	if !ok {
		return nil, nil
	}
	return &schedule, nil
}

func (*deliveryToolRepository) ScheduleOccurrences(context.Context, int64, int64, int) ([]personal.ScheduleOccurrence, error) {
	return []personal.ScheduleOccurrence{}, nil
}

func (*deliveryToolRepository) ScheduleSuggestions(context.Context, int64) ([]personal.ScheduleSuggestion, error) {
	return []personal.ScheduleSuggestion{}, nil
}

func (r *deliveryToolRepository) EventDeliveries(_ context.Context, eventID int64) ([]personal.DeliveryIntent, error) {
	return append([]personal.DeliveryIntent(nil), r.eventDeliveries[eventID]...), nil
}

func (r *deliveryToolRepository) CancelTask(ctx context.Context, eventID, taskID, revision int64) (personal.Task, error) {
	r.cancelCalls++
	task, err := r.toolRepo.CancelTask(ctx, eventID, taskID, revision)
	if err != nil {
		return personal.Task{}, err
	}
	intent := personal.DeliveryIntent{
		ID:      r.nextDeliveryID + 1,
		EventID: eventID,
		Purpose: personal.DeliveryControl,
		State:   personal.DeliveryIntentPending,
		Draft:   personal.DeliveryDraft{Text: "Store-confirmed task cancellation.", Attachments: []string{}},
		Scopes:  []personal.DeliveryScope{{TaskID: taskID, Revision: revision}},
	}
	r.nextDeliveryID = intent.ID
	r.addIntent(intent)
	return task, nil
}

func (r *deliveryToolRepository) ReviseTask(ctx context.Context, eventID int64, key string, taskID, revision int64, input personal.TaskInput) (personal.Task, personal.Run, error) {
	if r.reviseErr != nil {
		return personal.Task{}, personal.Run{}, r.reviseErr
	}
	return r.toolRepo.ReviseTask(ctx, eventID, key, taskID, revision, input)
}

func TestDeliveryControlsAreUnavailableOutsideDirectUserEvents(t *testing.T) {
	for _, kind := range []personal.EventKind{personal.EventTask, personal.EventMaterial} {
		t.Run(string(kind), func(t *testing.T) {
			repo := newDeliveryToolRepository()
			_, tools := newTaskActionTools(t, repo, personal.Event{ID: 21, Kind: kind, TaskID: 7}, personal.TurnInput{})
			calls := []struct {
				name string
				args any
			}{
				{"list_deliveries", personal.DeliveryQuery{}},
				{"get_delivery", getDeliveryArgs{DeliveryID: 1}},
				{"stop_delivery", stopDeliveryArgs{TaskID: 7}},
				{"resume_task_delivery", resumeTaskDeliveryArgs{TaskID: 7}},
				{"resend_delivery", resendDeliveryArgs{DeliveryID: 1}},
			}
			for _, call := range calls {
				reply := invokeActionTool(t, tools, call.name, call.args)
				if reply.OK || !strings.Contains(reply.Error, "direct user event") {
					t.Errorf("%s returned %+v, want direct-user authorization denial", call.name, reply)
				}
			}
			if len(repo.controlCalls) != 0 || len(repo.deliveryQueries) != 0 || repo.queryDeliveryCalls != 0 {
				t.Fatalf("unauthorized delivery tools accessed storage: controls=%+v delivery=%v queries=%d", repo.controlCalls, repo.deliveryQueries, repo.queryDeliveryCalls)
			}
		})
	}
}

func TestDeliveryControlOperationsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		args    any
		request personal.DeliveryControlRequest
	}{
		{
			name: "stop task scope", tool: "stop_delivery", args: stopDeliveryArgs{TaskID: 7},
			request: personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: 7},
		},
		{
			name: "resume future only", tool: "resume_task_delivery", args: resumeTaskDeliveryArgs{TaskID: 7},
			request: personal.DeliveryControlRequest{Action: personal.DeliveryResume, TaskID: 7},
		},
		{
			name: "resend creates history", tool: "resend_delivery", args: resendDeliveryArgs{DeliveryID: 50},
			request: personal.DeliveryControlRequest{Action: personal.DeliveryResend, DeliveryID: 50},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newDeliveryToolRepository()
			repo.tasks[7] = personal.Task{ID: 7, Title: "Report", Revision: 3}
			repo.deliveries[50] = personal.DeliveryIntent{ID: 50, Purpose: personal.DeliveryResult, Draft: personal.DeliveryDraft{Text: "Old report", Attachments: []string{"file-1"}}, Scopes: []personal.DeliveryScope{{TaskID: 7, Revision: 2}}}
			event := personal.Event{ID: 22, Kind: personal.EventUser}
			p, tools := newTaskActionTools(t, repo, event, personal.TurnInput{Tasks: []personal.Task{repo.tasks[7]}})
			reply := invokeActionTool(t, tools, tc.tool, tc.args)
			if !reply.OK || len(repo.controlCalls) != 1 || repo.controlCalls[0].request != tc.request {
				t.Fatalf("%s reply=%+v calls=%+v", tc.name, reply, repo.controlCalls)
			}
			if reply.Control == nil || reply.Delivery == nil || reply.Control.Request != tc.request || reply.Control.DeliveryID != reply.Delivery.ID {
				t.Fatalf("%s did not return Store control and saved delivery: %+v", tc.name, reply)
			}
			if len(p.controlDeliveries) != 1 || !p.persistedControl || !p.mustNotify {
				t.Fatalf("%s did not retain saved control delivery: %+v", tc.name, p)
			}
			if tc.request.Action == personal.DeliveryResend && (reply.Delivery.Purpose != personal.DeliveryHistorical || reply.Delivery.OriginalDeliveryID != 50 || reply.Delivery.Draft.Text != "Old report") {
				t.Fatalf("resend did not preserve original history: %+v", reply.Delivery)
			}
		})
	}

	t.Run("explicit resend after an unknown outcome creates new history", func(t *testing.T) {
		repo := newDeliveryToolRepository()
		repo.deliveries[50] = personal.DeliveryIntent{
			ID: 50, Purpose: personal.DeliveryResult, State: personal.DeliveryIntentRendered,
			Detail: "an earlier send may have been accepted", Draft: personal.DeliveryDraft{Text: "Original result", Attachments: []string{}},
		}
		p, tools := newTaskActionTools(t, repo, personal.Event{ID: 23, Kind: personal.EventUser}, personal.TurnInput{})
		reply := invokeActionTool(t, tools, "resend_delivery", resendDeliveryArgs{DeliveryID: 50})
		if !reply.OK || len(repo.controlCalls) != 1 || len(p.controlDeliveries) != 1 {
			t.Fatalf("explicit resend after unknown result reply=%+v; controls=%+v deliveries=%+v", reply, repo.controlCalls, p.controlDeliveries)
		}
		if reply.Delivery == nil || reply.Delivery.Purpose != personal.DeliveryHistorical || reply.Delivery.OriginalDeliveryID != 50 || repo.deliveries[50].Detail != "an earlier send may have been accepted" {
			t.Fatalf("explicit resend did not preserve the original uncertain record: original=%+v resend=%+v", repo.deliveries[50], reply.Delivery)
		}
	})

	t.Run("store failures are tracked for each control operation", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			tool string
			args any
		}{
			{name: "stop", tool: "stop_delivery", args: stopDeliveryArgs{TaskID: 7}},
			{name: "resume", tool: "resume_task_delivery", args: resumeTaskDeliveryArgs{TaskID: 7}},
			{name: "resend", tool: "resend_delivery", args: resendDeliveryArgs{DeliveryID: 50}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				repo := newDeliveryToolRepository()
				repo.controlErr = errors.New("store rejected delivery control")
				repo.deliveries[50] = personal.DeliveryIntent{ID: 50, Purpose: personal.DeliveryResult, Draft: personal.DeliveryDraft{Text: "Original"}}
				p, tools := newTaskActionTools(t, repo, personal.Event{ID: 231, Kind: personal.EventUser}, personal.TurnInput{})
				reply := invokeActionTool(t, tools, tc.tool, tc.args)
				if reply.OK || !strings.Contains(reply.Error, "store rejected") || p.persistedControl || len(p.controlDeliveries) != 0 {
					t.Fatalf("%s store failure reply=%+v; persisted=%t intents=%+v", tc.name, reply, p.persistedControl, p.controlDeliveries)
				}
				if len(p.unresolvedTaskActions()) != 1 {
					t.Fatalf("%s failure was not tracked: %+v", tc.name, p.unresolvedTaskActions())
				}
			})
		}
	})

	t.Run("stop requires exactly one target", func(t *testing.T) {
		repo := newDeliveryToolRepository()
		p, tools := newTaskActionTools(t, repo, personal.Event{ID: 24, Kind: personal.EventUser}, personal.TurnInput{})
		reply := invokeActionTool(t, tools, "stop_delivery", stopDeliveryArgs{TaskID: 7, DeliveryID: 50})
		if reply.OK || !strings.Contains(reply.Error, "exactly one") || len(repo.controlCalls) != 0 || len(p.unresolvedTaskActions()) != 1 {
			t.Fatalf("invalid stop target reply=%+v controls=%+v failures=%+v", reply, repo.controlCalls, p.unresolvedTaskActions())
		}
	})
}

func TestThinkControlRecoversStoredConfirmationAndPreservesOtherTaskFailure(t *testing.T) {
	repo := newDeliveryToolRepository()
	failedTask := personal.Task{ID: 3, Title: "Old report", Goal: "Revise the report", Revision: 1}
	stoppedTask := personal.Task{ID: 8, Title: "New report", Goal: "Send the report", Revision: 2}
	repo.tasks[failedTask.ID], repo.tasks[stoppedTask.ID] = failedTask, stoppedTask
	repo.reviseErr = errors.New("temporary task update failure")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("update_task", reviseTaskArgs{TaskID: failedTask.ID, ExpectedRevision: failedTask.Revision, Goal: "A revised report"}, "update-fails"),
		testkit.ResponseTool("stop_delivery", stopDeliveryArgs{TaskID: stoppedTask.ID}, "stop-success"),
		{Status: "incomplete", Output: testkit.ResponseText(`{"text":"partial","reply_kind":"content","attachments":[]}`).Output},
	}}
	input := personal.TurnInput{Event: personal.Event{ID: 25, Kind: personal.EventUser}, Tasks: []personal.Task{failedTask, stoppedTask}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.controlCalls) != 1 || output.Text != "Store-confirmed delivery control." || output.Silent {
		t.Fatalf("control model recovery output=%+v calls=%+v", output, repo.controlCalls)
	}
	if output.RequiredFeedback == "" || !strings.Contains(output.RequiredFeedback, "Old report") || !strings.Contains(output.RequiredFeedback, "未能更新") {
		t.Fatalf("unrelated failed task action was lost: RequiredFeedback=%q", output.RequiredFeedback)
	}
	if !sameInt64Slice(output.TaskIDs, []int64{3, 8}) || output.TaskID != 0 {
		t.Fatalf("multi-task output scope = TaskID:%d TaskIDs:%v, want 0 and sorted [3 8]", output.TaskID, output.TaskIDs)
	}
	if len(repo.deliveryQueries) != 1 || repo.deliveryQueries[0] != repo.eventDeliveries[25][0].ID {
		t.Fatalf("did not reload persisted control body: queries=%v intents=%+v", repo.deliveryQueries, repo.eventDeliveries[25])
	}
}

func TestCancelTaskDoesNotInvokeDeliveryStopAndUsesStoreConfirmation(t *testing.T) {
	repo := newDeliveryToolRepository()
	task := personal.Task{ID: 7, Title: "Report", Goal: "Generate report", Revision: 2}
	repo.tasks[task.ID] = task
	repo.addIntent(personal.DeliveryIntent{ID: 100, EventID: 26, Purpose: personal.DeliveryControl, Draft: personal.DeliveryDraft{Text: "Earlier control confirmation."}})
	p, tools := newTaskActionTools(t, repo, personal.Event{ID: 26, Kind: personal.EventUser}, personal.TurnInput{Tasks: []personal.Task{task}})
	reply := invokeActionTool(t, tools, "cancel_task", cancelTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision})
	if !reply.OK || repo.cancelCalls != 1 || len(repo.controlCalls) != 0 {
		t.Fatalf("cancel task incorrectly invoked delivery stop: reply=%+v cancels=%d controls=%+v", reply, repo.cancelCalls, repo.controlCalls)
	}
	if !p.persistedControl || len(p.controlDeliveries) != 2 || p.controlDeliveries[1].Draft.Text != "Store-confirmed task cancellation." {
		t.Fatalf("cancel task did not retain Store confirmation: persisted=%t deliveries=%+v", p.persistedControl, p.controlDeliveries)
	}
	if reply.Deliveries == nil || len(reply.Deliveries.Items) != 2 || reply.Deliveries.Items[0].ID != 101 || reply.Deliveries.Items[1].ID != 100 {
		t.Fatalf("cancel result did not return this event's control confirmations in descending ID order: %+v", reply.Deliveries)
	}
}

func TestListDeliveriesReturnsCardsWithoutMutatingRepositorySnapshot(t *testing.T) {
	receipt := &channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner", MessageID: "accepted-remote-id"}
	repo := newDeliveryToolRepository()
	repo.queryPage = personal.DeliveryPage{Items: []personal.DeliveryIntent{{
		ID: 44, EventID: 30, Purpose: personal.DeliveryResult,
		Draft:  personal.DeliveryDraft{Text: "full draft body", Attachments: []string{"file-1"}},
		Scopes: []personal.DeliveryScope{{TaskID: 7, Revision: 3}},
		Progress: &personal.DeliveryProgress{AcceptedItems: 1, Items: []personal.DeliveryItem{{
			OutboxID: 90, DeliveryPart: 0, Text: "accepted text item", Status: "sent", Receipt: receipt, Attempts: 1,
		}}},
	}}}
	p, tools := newTaskActionTools(t, repo, personal.Event{ID: 30, Kind: personal.EventUser}, personal.TurnInput{})
	reply := invokeActionTool(t, tools, "list_deliveries", personal.DeliveryQuery{})
	if !reply.OK || reply.Deliveries == nil || len(reply.Deliveries.Items) != 1 {
		t.Fatalf("list delivery query reply=%+v", reply)
	}
	card := reply.Deliveries.Items[0]
	if !card.ContentOmitted || card.Draft.Text != "" || card.Progress == nil || card.Progress.Items[0].Text != "" {
		t.Fatalf("list did not return a compact delivery card: %+v", card)
	}
	if card.ID != 44 || card.Scopes[0] != (personal.DeliveryScope{TaskID: 7, Revision: 3}) || card.Progress.Items[0].OutboxID != 90 || card.Progress.Items[0].Status != "sent" || card.Progress.Items[0].Receipt == nil || card.Progress.Items[0].Receipt.MessageID != "accepted-remote-id" {
		t.Fatalf("delivery card dropped progress identity or receipt: %+v", card)
	}
	original := repo.queryPage.Items[0]
	if original.Draft.Text != "full draft body" || original.Progress.Items[0].Text != "accepted text item" || original.Progress.Items[0].Receipt == nil || original.Progress.Items[0].Receipt.MessageID != "accepted-remote-id" {
		t.Fatalf("list card mutated the Repository snapshot: %+v", original)
	}
	if !p.deliveryQueried {
		t.Fatal("successful list query was not recorded")
	}
}

func TestThinkDeliveryQueryPreservesAllTaskScopesSorted(t *testing.T) {
	repo := newDeliveryToolRepository()
	repo.queryPage = personal.DeliveryPage{
		Items: []personal.DeliveryIntent{
			{ID: 40, Scopes: []personal.DeliveryScope{{TaskID: 8, Revision: 1}}},
			{ID: 39, Scopes: []personal.DeliveryScope{{TaskID: 3, Revision: 2}}},
		},
		StoppedTaskIDs: []int64{5},
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("list_deliveries", personal.DeliveryQuery{}, "list-deliveries"),
		testkit.ResponseText(`{"text":"There are two delivery records.","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 27, Kind: personal.EventUser}})
	if err != nil {
		t.Fatal(err)
	}
	if !sameInt64Slice(output.TaskIDs, []int64{3, 5, 8}) || output.TaskID != 0 {
		t.Fatalf("delivery query output scope = TaskID:%d TaskIDs:%v, want 0 and sorted [3 5 8]", output.TaskID, output.TaskIDs)
	}
	if output.Purpose != personal.DeliveryControl {
		t.Fatalf("delivery query with a stopped task has purpose %q, want control so the status can be reported", output.Purpose)
	}
}

func TestThinkGetDeliveryUsesControlPurposeAfterStop(t *testing.T) {
	repo := newDeliveryToolRepository()
	repo.deliveries[44] = personal.DeliveryIntent{
		ID:      44,
		Purpose: personal.DeliveryResult,
		State:   personal.DeliveryIntentSuppressed,
		Draft:   personal.DeliveryDraft{Text: "Saved but stopped", Attachments: []string{}},
		Scopes:  []personal.DeliveryScope{{TaskID: 6, Revision: 2}},
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_delivery", getDeliveryArgs{DeliveryID: 44}, "get-stopped-delivery"),
		testkit.ResponseText(`{"text":"This delivery was suppressed.","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 28, Kind: personal.EventUser}})
	if err != nil {
		t.Fatal(err)
	}
	if output.Purpose != personal.DeliveryControl || !sameInt64Slice(output.TaskIDs, []int64{6}) {
		t.Fatalf("get delivery output purpose=%q task IDs=%v, want control and stopped task 6", output.Purpose, output.TaskIDs)
	}
}

func TestThinkCommittedControlIgnoresFinalModelAttachmentChoice(t *testing.T) {
	repo := newDeliveryToolRepository()
	repo.controlDraft = &personal.DeliveryDraft{Text: "Saved control confirmation.", Attachments: []string{"stored-file"}}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("stop_delivery", stopDeliveryArgs{TaskID: 6}, "stop-with-fixed-draft"),
		testkit.ResponseText(`{"text":"Different final text","reply_kind":"content","attachments":["not-a-saved-file"]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 30, Kind: personal.EventUser}})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "Saved control confirmation." || !sameStringSlice(output.Attachments, []string{"stored-file"}) || output.SupplementalReply != nil {
		t.Fatalf("final model overrode committed control draft: text=%q attachments=%v", output.Text, output.Attachments)
	}
}

func TestCancelTaskAndQueryScheduleKeepSupplementalScopeSeparate(t *testing.T) {
	repo := newDeliveryToolRepository()
	task := personal.Task{ID: 7, Title: "Report", Goal: "Generate report", Revision: 2}
	repo.tasks[task.ID] = task
	repo.schedules[45] = personal.Schedule{ID: 45, Revision: 3, ScheduleDefinition: personal.ScheduleDefinition{Title: "Morning plan"}}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("cancel_task", cancelTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision}, "cancel-task-a"),
		testkit.ResponseTool("get_schedule", getScheduleArgs{ScheduleID: 45}, "query-schedule-b"),
		testkit.ResponseText(`{"text":"Requested plan details.","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 31, Kind: personal.EventUser}, Tasks: []personal.Task{task},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "Store-confirmed task cancellation." || output.SupplementalReply == nil {
		t.Fatalf("control plus schedule query output=%+v", output)
	}
	supplement := output.SupplementalReply
	if supplement.Draft.Text != "Requested plan details." || supplement.ScheduleID != 45 || len(supplement.TaskIDs) != 0 || supplement.Purpose != personal.DeliveryControl {
		t.Fatalf("supplemental schedule reply=%+v; control Task scope leaked or schedule scope missing", supplement)
	}
}

func TestStopTaskAndQueryAnotherTaskUseOnlyQueriedTaskInSupplement(t *testing.T) {
	repo := newDeliveryToolRepository()
	stopped := personal.Task{ID: 7, Title: "Stopped report", Goal: "Generate report", Revision: 2}
	queried := personal.Task{ID: 8, Title: "Other report", Goal: "Inspect the other report", Revision: 3}
	repo.tasks[stopped.ID], repo.tasks[queried.ID] = stopped, queried
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("stop_delivery", stopDeliveryArgs{TaskID: stopped.ID}, "stop-task-a"),
		testkit.ResponseTool("get_task", taskArgs{TaskID: queried.ID}, "query-task-b"),
		testkit.ResponseText(`{"text":"Requested task details.","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 32, Kind: personal.EventUser}, Tasks: []personal.Task{stopped, queried},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "Store-confirmed delivery control." || output.SupplementalReply == nil {
		t.Fatalf("control plus task query output=%+v", output)
	}
	supplement := output.SupplementalReply
	if supplement.Draft.Text != "Requested task details." || !sameInt64Slice(supplement.TaskIDs, []int64{queried.ID}) || supplement.ScheduleID != 0 || supplement.Purpose != personal.DeliveryControl {
		t.Fatalf("supplemental task reply=%+v; stopped Task scope leaked or queried scope missing", supplement)
	}
}

func TestCommittedControlAndQueryModelFailureOnlyReportsIncompleteFeedback(t *testing.T) {
	repo := newDeliveryToolRepository()
	task := personal.Task{ID: 7, Title: "Report", Goal: "Generate report", Revision: 2}
	repo.tasks[task.ID] = task
	repo.schedules[45] = personal.Schedule{ID: 45, Revision: 3, ScheduleDefinition: personal.ScheduleDefinition{Title: "Morning plan"}}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("cancel_task", cancelTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision}, "cancel-before-query-failure"),
		testkit.ResponseTool("get_schedule", getScheduleArgs{ScheduleID: 45}, "query-before-final-failure"),
		{Status: "incomplete", Output: testkit.ResponseText(`{"text":"partial","reply_kind":"content","attachments":[]}`).Output},
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 33, Kind: personal.EventUser}, Tasks: []personal.Task{task},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "Store-confirmed task cancellation." || output.SupplementalReply != nil || !strings.Contains(output.RequiredFeedback, "其他查询或回复未能完成") {
		t.Fatalf("model failure lost committed control or fabricated query result: %+v", output)
	}
}

func TestDeliveryQueryPurposeDoesNotOverrideSuccessfulTaskCreation(t *testing.T) {
	repo := newDeliveryToolRepository()
	repo.queryPage = personal.DeliveryPage{StoppedTaskIDs: []int64{5}}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("list_deliveries", personal.DeliveryQuery{}, "list-before-create"),
		testkit.ResponseTool("create_task", createTaskArgs{Title: "New report", Goal: "Prepare the new report"}, "create-after-list"),
		testkit.ResponseText(`{"text":"The new task is queued.","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 29, Kind: personal.EventUser}})
	if err != nil {
		t.Fatal(err)
	}
	if output.Purpose != "" || !sameInt64Slice(output.TaskIDs, []int64{5, 90}) {
		t.Fatalf("query plus task creation purpose=%q task IDs=%v; want default result purpose and both scopes", output.Purpose, output.TaskIDs)
	}
}
