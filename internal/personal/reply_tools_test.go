package personal

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestSetUserQuestionsReplacesClearsAndTreatsOmissionAsNoOp(t *testing.T) {
	owner := &personalTools{event: Event{ID: 1, Kind: EventUser}}
	tools, err := owner.replyTools()
	if err != nil {
		t.Fatal(err)
	}
	if reply := invokeActionTool(t, tools, "set_user_questions", map[string]any{}); !reply.OK || len(owner.questions) != 0 {
		t.Fatalf("empty object invented questions: reply=%+v questions=%v", reply, owner.questions)
	}

	reply := invokeActionTool(t, tools, "set_user_questions", map[string]any{"questions": []string{"  选择哪个目标？  ", "需要什么授权？", "选择哪个目标？"}})
	want := []string{"选择哪个目标？", "需要什么授权？"}
	if !reply.OK || !slices.Equal(owner.questions, want) {
		t.Fatalf("questions=%+v reply=%+v, want ordered unique questions %v", owner.questions, reply, want)
	}
	for _, args := range []map[string]any{{}, {"questions": nil}} {
		if reply := invokeActionTool(t, tools, "set_user_questions", args); !reply.OK || !slices.Equal(owner.questions, want) {
			t.Fatalf("omitted/null questions changed the list: reply=%+v questions=%v", reply, owner.questions)
		}
	}
	reply = invokeActionTool(t, tools, "set_user_questions", map[string]any{"questions": []string{"替换后的问题"}})
	if !reply.OK || !slices.Equal(owner.questions, []string{"替换后的问题"}) {
		t.Fatalf("replacement questions=%v reply=%+v", owner.questions, reply)
	}
	reply = invokeActionTool(t, tools, "set_user_questions", map[string]any{"questions": []string{}})
	if !reply.OK || len(owner.questions) != 0 || owner.requiredTaskReply() != "" {
		t.Fatalf("empty array did not clear pending questions: reply=%+v required=%q", reply, owner.requiredTaskReply())
	}
}

func TestSetUserQuestionsRejectsInvalidArrayAtomicallyAndCallbacks(t *testing.T) {
	owner := &personalTools{event: Event{ID: 1, Kind: EventUser}, questions: []string{"Existing question?"}}
	tools, err := owner.replyTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]string{{"Replacement?", " "}, {"Replacement?", strings.Repeat("x", maxTaskChars+1)}} {
		reply := invokeActionTool(t, tools, "set_user_questions", map[string]any{"questions": invalid})
		if reply.OK || !slices.Equal(owner.questions, []string{"Existing question?"}) {
			t.Fatalf("invalid list partially changed questions: reply=%+v questions=%v", reply, owner.questions)
		}
	}
	callback := &personalTools{event: Event{ID: 2, Kind: EventTask}, questions: []string{"Keep this state"}}
	callbackTools, err := callback.replyTools()
	if err != nil {
		t.Fatal(err)
	}
	reply := invokeActionTool(t, callbackTools, "set_user_questions", map[string]any{"questions": []string{}})
	if reply.OK || !slices.Equal(callback.questions, []string{"Keep this state"}) || callback.mustNotify {
		t.Fatalf("callback changed questions or forced notification: reply=%+v questions=%v mustNotify=%t", reply, callback.questions, callback.mustNotify)
	}
}

func TestClearingQuestionsKeepsUnresolvedTaskFailureReply(t *testing.T) {
	owner := &personalTools{
		event:     Event{ID: 1, Kind: EventUser, Text: "Please queue the report."},
		questions: []string{"Which report?"},
		taskActions: map[string]*taskActionState{
			"create-report": {failure: taskActionFailure{Operation: "create_task", Target: "Report", Error: "missing goal"}},
		},
		taskActionOrder: []string{"create-report"},
	}
	tools, err := owner.replyTools()
	if err != nil {
		t.Fatal(err)
	}
	if reply := invokeActionTool(t, tools, "set_user_questions", map[string]any{"questions": []string{}}); !reply.OK {
		t.Fatalf("clear questions reply=%+v", reply)
	}
	response, err := (&MainAgent{}).selectReply(context.Background(), TurnInput{Event: owner.event}, owner,
		`{"text":"已安排。","reply_kind":"receipt","attachments":[]}`, nil, nil)
	if err != nil || response.ReplyKind != replyContent || !strings.Contains(response.Text, "未能创建任务") || strings.Contains(response.Text, "Which report?") {
		t.Fatalf("clearing questions hid or retained the wrong reply: response=%+v error=%v", response, err)
	}
}
