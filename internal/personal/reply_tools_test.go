package personal

import (
	"testing"
)

func TestAskUserRecordsOnlyDirectUserQuestions(t *testing.T) {
	for _, kind := range []EventKind{EventUser, EventTask} {
		t.Run(string(kind), func(t *testing.T) {
			owner := &personalTools{event: Event{ID: 1, Kind: kind}}
			tools, err := owner.replyTools()
			if err != nil {
				t.Fatal(err)
			}
			blank := invokeActionTool(t, tools, "ask_user", askUserArgs{Question: " "})
			if blank.OK || len(owner.questions) != 0 {
				t.Fatal("blank question created a reply obligation")
			}
			const question = "两个任务中，你希望把材料补给哪一个？"
			for i := 0; i < 2; i++ {
				reply := invokeActionTool(t, tools, "ask_user", askUserArgs{Question: question})
				if reply.OK != (kind == EventUser) {
					t.Fatalf("question authorization: %+v", reply)
				}
			}
			if kind == EventUser {
				if len(owner.questions) != 1 || owner.requiredTaskReply() != question {
					t.Fatalf("required question=%q", owner.requiredTaskReply())
				}
			} else if len(owner.questions) != 0 || owner.mustNotify {
				t.Fatal("rejected callback question forced a reply")
			}
		})
	}
}
