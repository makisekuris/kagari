package personal

import (
	"context"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestReferencedTasksPreserveMultipleTasksInRuntimeContext(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 41, "请分别核对两个问题")
	first, _, err := s.CreateTask(ctx, event.ID, "create-first", "第一项", TaskInput{Goal: "核对第一项"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.CreateTask(ctx, event.ID, "create-second", "第二项", TaskInput{Goal: "核对第二项"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{Silent: true}, nil)
	if queued, err := s.QueueWaitingReply(ctx, time.Now(), 0); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	waiting, err := s.ClaimDelivery(ctx)
	if err != nil || waiting == nil {
		t.Fatalf("ClaimDelivery() = (%+v,%v)", waiting, err)
	}
	if waiting.Text != "任务已排队，有结果后我会继续回复。" {
		t.Fatalf("waiting delivery = %q", waiting.Text)
	}
	const waitingMessageID = "wait-receipt-41"
	receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: waitingMessageID}
	if err := s.UpdateDelivery(ctx, waiting.ID, "sent", receipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	replyInput := sourceInput("reply-to-wait-41", "Which result covers each question?")
	replyInput.ReplyToID = waitingMessageID
	replyID := acceptInput(t, s, "2", replyInput)
	reply, err := s.ClaimEvent(ctx)
	if err != nil || reply == nil || reply.ID != replyID {
		t.Fatalf("ClaimEvent() = (%+v,%v), want reply %d", reply, err, replyID)
	}
	if reply.TaskID != 0 {
		t.Fatalf("reply guessed a single task ID %d", reply.TaskID)
	}

	input, err := (&Runtime{Store: s}).context(ctx, *reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.ReferencedTaskIDs) != 2 || input.ReferencedTaskIDs[0] != first.ID || input.ReferencedTaskIDs[1] != second.ID {
		t.Fatalf("referenced task candidates = %v, want [%d %d]", input.ReferencedTaskIDs, first.ID, second.ID)
	}
	if len(input.Tasks) < 2 {
		t.Fatalf("runtime context omitted task details for candidates: %+v", input.Tasks)
	}
}

func TestReferencedTasksFindTaskThroughMergedSecondInput(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	current := openUserEvent(t, s, 1, 51, "把后续链接纳入同一项研究")
	secondOriginalID := acceptInput(t, s, "2", sourceInput("merged-original-52", "https://example.com/reference"))
	task, _, err := s.CreateTask(ctx, current.ID, "create-research", "资料核对", TaskInput{Goal: "核对这份资料"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "add-reference", task.ID, task.Revision, []int64{current.ID, secondOriginalID}); err != nil {
		t.Fatal(err)
	}
	var currentGroup, secondGroup int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, current.ID).Scan(&currentGroup); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, secondOriginalID).Scan(&secondGroup); err != nil || currentGroup != secondGroup {
		t.Fatalf("reply groups for original inputs = (%d,%d), %v", currentGroup, secondGroup, err)
	}
	finishEvent(t, s, current, TurnOutput{TaskID: task.ID, Silent: true}, nil)

	replyInput := sourceInput("reply-to-merged-52", "请说明这份链接的结论")
	replyInput.ReplyToID = "merged-original-52"
	replyID := acceptInput(t, s, "3", replyInput)
	reply, err := s.ClaimEvent(ctx)
	if err != nil || reply == nil || reply.ID != replyID || reply.TaskID != task.ID {
		t.Fatalf("reply to merged original = (%+v,%v), want task %d", reply, err, task.ID)
	}
	referenced, err := s.ReferencedTasks(ctx, reply.ID)
	if err != nil || len(referenced) != 1 || referenced[0].ID != task.ID {
		t.Fatalf("ReferencedTasks() = (%+v,%v), want task %d", referenced, err, task.ID)
	}
	input, err := (&Runtime{Store: s}).context(ctx, *reply)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range input.Tasks {
		if candidate.ID == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("runtime context omitted task linked through the second original input: %+v", input.Tasks)
	}
}

func TestReferencedTasksRequireMatchingSourceIdentity(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		account string
		sender  string
	}{
		{name: "channel", channel: "weixin", account: "wx-bot", sender: "test-owner"},
		{name: "account", channel: "telegram", account: "other-bot", sender: "test-owner"},
		{name: "sender", channel: "telegram", account: "test-bot", sender: "other-owner"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			source := openUserEvent(t, s, 1, 61, "請處理這個來源")
			task, _, err := s.CreateTask(ctx, source.ID, "create-source", "来源任务", TaskInput{Goal: "核对来源"})
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, source, TurnOutput{Silent: true}, nil)
			if err := s.BindChannel(ctx, tc.channel, tc.account, tc.sender, tc.channel == "telegram"); err != nil {
				t.Fatal(err)
			}
			replyInput := channel.Input{
				Channel: tc.channel, Account: tc.account, Sender: tc.sender,
				MessageID: "reply-identity-" + tc.name, ReplyToID: source.MessageID, Text: "这条回复不属于原来源身份",
			}
			replyID := acceptInput(t, s, "2", replyInput)
			reply, err := s.ClaimEvent(ctx)
			if err != nil || reply == nil || reply.ID != replyID {
				t.Fatalf("ClaimEvent() = (%+v,%v), want reply %d", reply, err, replyID)
			}
			if reply.TaskID != 0 {
				t.Fatalf("mismatched %s associated task %d, want none", tc.name, reply.TaskID)
			}
			referenced, err := s.ReferencedTasks(ctx, reply.ID)
			if err != nil || len(referenced) != 0 {
				t.Fatalf("ReferencedTasks() for mismatched %s = (%+v,%v), want no tasks (source task %d)", tc.name, referenced, err, task.ID)
			}
			input, err := (&Runtime{Store: s}).context(ctx, *reply)
			if err != nil {
				t.Fatal(err)
			}
			if len(input.ReferencedTaskIDs) != 0 {
				t.Fatalf("runtime context matched a different %s: %v", tc.name, input.ReferencedTaskIDs)
			}
		})
	}
}
