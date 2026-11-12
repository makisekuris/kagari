package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func invokeTaskInputTool(t *testing.T, tools []tool.BaseTool, args addTaskInputArgs) (addTaskInputReply, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != "add_task_input" {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatal("add_task_input is not invokable")
		}
		output, err := invokable.InvokableRun(context.Background(), string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		var reply addTaskInputReply
		if err := json.Unmarshal([]byte(output), &reply); err != nil {
			t.Fatalf("decode add_task_input reply %q: %v", output, err)
		}
		return reply, output
	}
	t.Fatal("add_task_input tool not found")
	return addTaskInputReply{}, ""
}

func TestInputToolsReplyWithSelectedOriginalMessages(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Hour)
	event := testkit.OpenUserEvent(t, s, 40, 140, "Please add the current details to this task")
	selectedID := testkit.AcceptInput(t, s, "41", testkit.SourceInput("141", "https://example.com/selected"))
	unselectedID := testkit.AcceptInput(t, s, "42", testkit.SourceInput("142", "https://example.org/unselected"))
	task, _, err := s.CreateTask(ctx, event.ID, "create-tool-input", "Research", personal.TaskInput{Goal: "Research the pages"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingInputs(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := &personalTools{store: s, event: event, input: personal.TurnInput{PendingInputs: pending}, taskIDs: make(map[int64]struct{})}
	tools, err := p.inputTools()
	if err != nil || len(tools) != 1 {
		t.Fatalf("inputTools()=(%d,%v), want one tool", len(tools), err)
	}
	reply, raw := invokeTaskInputTool(t, tools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, selectedID},
	})
	if !reply.OK || reply.Task == nil || reply.Task.ID != task.ID || len(reply.Inputs) != 2 || utf8.RuneCountInString(raw) > personal.MaxTaskChars {
		t.Fatalf("add_task_input reply=(%+v), chars=%d", reply, utf8.RuneCountInString(raw))
	}
	texts := []string{reply.Inputs[0].Text, reply.Inputs[1].Text}
	if !strings.Contains(strings.Join(texts, "\n"), event.Text) || !strings.Contains(strings.Join(texts, "\n"), "https://example.com/selected") ||
		strings.Contains(strings.Join(texts, "\n"), "https://example.org/unselected") {
		t.Fatalf("tool returned the wrong original messages: %+v", reply.Inputs)
	}
	if p.taskID() != task.ID {
		t.Fatalf("task association=%d, want %d", p.taskID(), task.ID)
	}
	pendingAfter, err := s.PendingInputs(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	pendingIDs := make(map[int64]bool, len(pendingAfter))
	for _, pending := range pendingAfter {
		pendingIDs[pending.ID] = true
	}
	if pendingIDs[selectedID] || !pendingIDs[unselectedID] {
		t.Fatalf("selected input remains pending or unselected input was consumed: %v", pendingIDs)
	}
}

func TestInputToolsRejectInvalidSelectionAndNonUserCallback(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Hour)
	event := testkit.OpenUserEvent(t, s, 50, 150, "Add current details")
	commandID := testkit.AcceptInput(t, s, "51", testkit.SourceInput("151", "/continue_task https://example.com"))
	task, _, err := s.CreateTask(ctx, event.ID, "create-invalid-tool-input", "Research", personal.TaskInput{Goal: "Research this"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.PendingInputs(ctx, event.ID)
	if err != nil || len(snapshot) != 0 {
		t.Fatalf("pending snapshot=(%+v,%v), want no command candidates", snapshot, err)
	}
	p := &personalTools{store: s, event: event, input: personal.TurnInput{PendingInputs: snapshot}, taskIDs: make(map[int64]struct{})}
	tools, err := p.inputTools()
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, commandID},
	})
	if reply.OK || !strings.Contains(reply.Error, "pending URL input") {
		t.Fatalf("command input was accepted: %+v", reply)
	}
	arrivedAfterSnapshot := testkit.AcceptInput(t, s, "52", testkit.SourceInput("152", "https://example.com/arrived-later"))
	lateReply, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, arrivedAfterSnapshot},
	})
	if lateReply.OK || !strings.Contains(lateReply.Error, "pending URL input") {
		t.Fatalf("tool selected an event outside its turn snapshot: %+v", lateReply)
	}
	callback := personal.Event{ID: 999, Kind: personal.EventTask, TaskID: task.ID}
	callbackTools, err := (&personalTools{store: s, event: callback, taskIDs: map[int64]struct{}{task.ID: {}}}).inputTools()
	if err != nil {
		t.Fatal(err)
	}
	callbackReply, _ := invokeTaskInputTool(t, callbackTools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{callback.ID},
	})
	if callbackReply.OK || !strings.Contains(callbackReply.Error, "direct user event") {
		t.Fatalf("task callback was allowed to add inputs: %+v", callbackReply)
	}
}
