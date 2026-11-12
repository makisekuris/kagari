package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestThinkReceivesCompactPartialDeliveryFactsAfterLongHistoryPruning(t *testing.T) {
	repo := newDeliveryToolRepository()
	receipt := &channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "remote-accepted-part"}
	next := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	partial := personal.DeliveryIntent{
		ID: 71, EventID: 9, Purpose: personal.DeliveryResult, State: personal.DeliveryIntentRendered,
		Draft:  personal.DeliveryDraft{Text: "DRAFT-BODY-" + strings.Repeat("d", 25_000), Attachments: []string{"file-result"}},
		Scopes: []personal.DeliveryScope{{TaskID: 7, Revision: 4, RunID: 19}},
		Progress: &personal.DeliveryProgress{AcceptedItems: 1, FullyAccepted: false, Items: []personal.DeliveryItem{
			{OutboxID: 501, DeliveryPart: 0, Text: "SENT-BODY-" + strings.Repeat("s", 5_000), Status: "sent", Receipt: receipt, Attempts: 1},
			{OutboxID: 502, DeliveryPart: 1, Text: "PENDING-BODY-" + strings.Repeat("p", 5_000), Status: "pending", AttachmentID: "file-pending", Attempts: 2, NextAttemptAt: next},
			{OutboxID: 503, DeliveryPart: 2, AttachmentID: "file-failed", Status: "failed", Detail: "channel rejected the attachment", Attempts: 1},
			{OutboxID: 504, DeliveryPart: 3, Text: "UNCERTAIN-BODY-" + strings.Repeat("u", 5_000), Status: "uncertain", Detail: "send ended before a receipt was saved", Attempts: 1},
			{OutboxID: 505, DeliveryPart: 4, AttachmentID: "file-suppressed", Status: "suppressed", Detail: "later item stopped after uncertain result", Attempts: 0},
		}},
	}
	pending := personal.DeliveryIntent{
		ID: 70, EventID: 8, Purpose: personal.DeliveryStatus, State: personal.DeliveryIntentPending,
		Draft:    personal.DeliveryDraft{Text: "PENDING-QUESTION-" + strings.Repeat("q", 25_000), Attachments: []string{}},
		Scopes:   []personal.DeliveryScope{{TaskID: 7, Revision: 3}},
		Progress: &personal.DeliveryProgress{Items: []personal.DeliveryItem{{OutboxID: 490, DeliveryPart: 0, Text: "QUESTION-ITEM-" + strings.Repeat("x", 5_000), Status: "pending"}}},
	}
	repo.addIntent(pending)
	repo.addIntent(partial)
	input := personal.TurnInput{
		Event: personal.Event{ID: 10, Kind: personal.EventUser, Text: "报告进度如何？"},
		History: []personal.Message{
			{HistoryID: 201, EventID: 8, Role: "assistant", TaskID: 7, Text: "HISTORY-BODY-" + strings.Repeat("h", 25_000), Delivery: &pending},
			{HistoryID: 202, EventID: 9, Role: "assistant", TaskID: 7, Text: "HISTORY-BODY-" + strings.Repeat("j", 25_000), Delivery: &partial},
		},
	}
	limits, err := inputbudget.NewLimits(100_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	instruction := personalInstruction("Persona style token.", "Profile facts token.", false)
	assembled, err := assembleTurn(context.Background(), input, instruction, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(assembled.History) != 2 || assembled.History[0].Text != "" || !assembled.History[0].TextOmitted || assembled.History[1].Text != "" || !assembled.History[1].TextOmitted {
		t.Fatalf("long delivery History bodies were not reduced to compact cards: %+v", assembled.History)
	}
	fullHistory := assembled
	fullHistory.History = input.History
	fullEstimate, err := estimateTurn(context.Background(), fullHistory, instruction, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fullEstimate.TotalBytes <= limits.Target {
		t.Fatalf("test data does not exercise the target budget: full estimate=%d target=%d", fullEstimate.TotalBytes, limits.Target)
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"有一部分已被渠道接受，其余部分尚未全部完成。","reply_kind":"content","attachments":[]}`),
	}}
	main := newMainAgentWithWindow(t, fixture, repo, 100_000, 2_000)
	if _, err := main.Think(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if input.History[0].Text == "" || input.History[0].Delivery.Draft.Text == "" || input.History[1].Delivery.Progress.Items[0].Text == "" {
		t.Fatal("context assembly mutated the original delivery History")
	}
	modelInput := fixtureRequestText(t, fixture.Requests[0])
	for _, want := range []string{
		`"history_id":201`, `"history_id":202`, `"text_omitted":true`, `"content_omitted":true`,
		`"outbox_id":501`, `"outbox_id":502`, `"outbox_id":503`, `"outbox_id":504`, `"outbox_id":505`, `"outbox_id":490`,
		`"message_id":"remote-accepted-part"`, `"accepted_items":1`, `"fully_accepted":false`,
		`"status":"pending"`, `"status":"failed"`, `"status":"uncertain"`, `"status":"suppressed"`, `"status":"sent"`,
		`"detail":"channel rejected the attachment"`, `"attachment_id":"file-failed"`,
		"does not prove the owner read it", "does not answer it or authorize a broader task", "reuse those results",
	} {
		if !strings.Contains(modelInput, want) {
			t.Errorf("model request omitted delivery fact or policy %q", want)
		}
	}
	for _, body := range []string{"DRAFT-BODY-", "PENDING-QUESTION-", "SENT-BODY-", "PENDING-BODY-", "UNCERTAIN-BODY-", "HISTORY-BODY-"} {
		if strings.Contains(modelInput, body) {
			t.Errorf("long generated body %q was not pruned from the model request", body)
		}
	}
}

func TestAssembleTurnRejectsDeliveryCardFactsThatExceedHardLimit(t *testing.T) {
	limits, err := inputbudget.NewLimits(16_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	input := personal.TurnInput{
		Event: personal.Event{ID: 11, Kind: personal.EventUser, Text: "What happened?"},
		History: []personal.Message{{HistoryID: 1, EventID: 1, Role: "assistant", Delivery: &personal.DeliveryIntent{
			ID: 3, State: personal.DeliveryIntentSuppressed,
			Detail: strings.Repeat("complete reason ", 2_000),
		}}},
	}
	_, err = assembleTurn(context.Background(), input, "instruction", nil, limits)
	var limitErr *inputbudget.ContextLimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != inputbudget.ErrorRequiredInputExceedsLimit {
		t.Fatalf("oversized required delivery card error=%v", err)
	}
}

func TestExactDeliveryReplyDoesNotFallBackToSameEventOrWrongSource(t *testing.T) {
	intent := func(id int64, outboxID int64, receiptID string) *personal.DeliveryIntent {
		return &personal.DeliveryIntent{ID: id, Progress: &personal.DeliveryProgress{Items: []personal.DeliveryItem{{OutboxID: outboxID, Receipt: &channel.Receipt{MessageID: receiptID}}}}}
	}

	t.Run("history identity is exact even for same event", func(t *testing.T) {
		input := personal.TurnInput{RepliedMessage: &personal.Message{HistoryID: 40, EventID: 9, Role: "assistant", Delivery: intent(5, 80, "receipt")}}
		if !isExplicitReply(input, personal.Message{HistoryID: 40, EventID: 9, Role: "assistant"}) {
			t.Fatal("exact History ID was not retained")
		}
		if isExplicitReply(input, personal.Message{HistoryID: 41, EventID: 9, Role: "assistant", Delivery: intent(5, 80, "receipt")}) {
			t.Fatal("same-event History was retained despite a different exact History ID")
		}
	})

	t.Run("delivery identity is exact when History ID is absent", func(t *testing.T) {
		input := personal.TurnInput{RepliedMessage: &personal.Message{EventID: 9, Role: "assistant", Delivery: intent(5, 80, "receipt")}}
		if !isExplicitReply(input, personal.Message{EventID: 9, Role: "assistant", Delivery: intent(5, 81, "other")}) {
			t.Fatal("matching delivery identity was not retained")
		}
		if isExplicitReply(input, personal.Message{EventID: 9, Role: "assistant", Delivery: intent(6, 80, "receipt")}) {
			t.Fatal("same-event message with a different Delivery ID was retained")
		}
	})

	t.Run("outbox identity wins over same-event fallback", func(t *testing.T) {
		input := personal.TurnInput{RepliedMessage: &personal.Message{EventID: 9, Role: "assistant", OutboxID: 80}}
		if !isExplicitReply(input, personal.Message{EventID: 12, Role: "assistant", Delivery: intent(5, 80, "receipt")}) {
			t.Fatal("History carrying the exact outbox item was not retained")
		}
		if isExplicitReply(input, personal.Message{EventID: 9, Role: "assistant", Delivery: intent(5, 81, "other")}) {
			t.Fatal("same-event message with another outbox item was retained")
		}
	})

	t.Run("reply receipt must match known source", func(t *testing.T) {
		input := personal.TurnInput{Event: personal.Event{ReplyToID: "same-id", Channel: "telegram", Account: "bot-a", Sender: "owner"}}
		matching := personal.Message{MessageID: "same-id", Channel: "telegram", Account: "bot-a", Sender: "owner"}
		if !isExplicitReply(input, matching) {
			t.Fatal("matching source was not retained")
		}
		for _, mismatch := range []personal.Message{
			{MessageID: "same-id", Channel: "discord", Account: "bot-a", Sender: "owner"},
			{MessageID: "same-id", Channel: "telegram", Account: "other-bot", Sender: "owner"},
			{MessageID: "same-id", Channel: "telegram", Account: "bot-a", Sender: "other-owner"},
			{Delivery: &personal.DeliveryIntent{ID: 7, Progress: &personal.DeliveryProgress{Items: []personal.DeliveryItem{{Receipt: &channel.Receipt{Channel: "discord", Account: "bot-a", Conversation: "owner", MessageID: "same-id"}}}}}},
		} {
			if isExplicitReply(input, mismatch) {
				t.Fatalf("reply with a known source mismatch was retained: %+v", mismatch)
			}
		}
		if !isExplicitReply(personal.TurnInput{Event: personal.Event{ReplyToID: "same-id"}}, personal.Message{MessageID: "same-id"}) {
			t.Fatal("empty source fields should still match when both are unknown")
		}
	})
}

func TestDeliveryHistoryPrioritizesEveryTaskScopeForBodyExpansion(t *testing.T) {
	input := personal.TurnInput{
		ReferencedTaskIDs: []int64{8},
		History: []personal.Message{
			{HistoryID: 1, Delivery: &personal.DeliveryIntent{ID: 1, Scopes: []personal.DeliveryScope{{TaskID: 3}, {TaskID: 4}}}},
			{HistoryID: 2, Delivery: &personal.DeliveryIntent{ID: 2, Scopes: []personal.DeliveryScope{{TaskID: 7}, {TaskID: 8}}}},
		},
	}
	order := deliveryHistoryExpansionOrder(input, map[int]int{0: 0, 1: 1}, nil)
	if len(order) != 2 || order[0] != 1 || order[1] != 0 {
		t.Fatalf("history with a matching secondary task scope did not rank first: %v", order)
	}
}

func TestThinkDoesNotExpandUnsentQuestionOnUnassociatedYesAndReusesSavedReport(t *testing.T) {
	repo := newDeliveryToolRepository()
	task := personal.Task{ID: 7, Title: "Comparison", Goal: "Compare three products", Revision: 4, Status: personal.TaskCompleted, Summary: "The saved report compares three products and recommends option C."}
	repo.tasks[task.ID] = task
	unsentQuestion := personal.DeliveryIntent{
		ID: 80, EventID: 6, Purpose: personal.DeliveryStatus, State: personal.DeliveryIntentPending,
		Draft:    personal.DeliveryDraft{Text: "Should I expand the comparison to overseas products?", Attachments: []string{}},
		Scopes:   []personal.DeliveryScope{{TaskID: task.ID, Revision: task.Revision}},
		Progress: &personal.DeliveryProgress{Items: []personal.DeliveryItem{{OutboxID: 801, DeliveryPart: 0, Status: "pending"}}},
	}
	repo.addIntent(unsentQuestion)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_delivery", getDeliveryArgs{DeliveryID: unsentQuestion.ID}, "inspect-unsent-question"),
		testkit.ResponseText(`{"text":"这个扩展问题尚未确认送达。你说的“可以”是指同意把比较范围扩展到海外产品吗？","reply_kind":"content","attachments":[]}`),
		testkit.ResponseTool("get_task", taskArgs{TaskID: task.ID}, "reuse-saved-report"),
		testkit.ResponseText(`{"text":"此前保存的报告比较了三款产品，并推荐了 C 方案。","reply_kind":"content","attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, repo)
	first, err := main.Think(context.Background(), personal.TurnInput{
		Event:   personal.Event{ID: 90, Kind: personal.EventUser, Text: "可以"},
		History: []personal.Message{{HistoryID: 60, EventID: 6, Role: "assistant", TaskID: task.ID, Text: unsentQuestion.Draft.Text, Delivery: &unsentQuestion}},
		Tasks:   []personal.Task{task},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Text, "尚未确认送达") || len(repo.continued) != 0 || len(repo.created) != 0 || len(repo.revisedKeys) != 0 {
		t.Fatalf("unassociated yes expanded an unsent question: output=%+v continued=%v created=%v revised=%v", first, repo.continued, repo.created, repo.revisedKeys)
	}
	if toolResult := fixtureRequestText(t, fixture.Requests[1]); !strings.Contains(toolResult, "Should I expand the comparison") || !strings.Contains(toolResult, `"status":"pending"`) {
		t.Fatalf("model did not inspect the exact pending question before clarification: %s", toolResult)
	}

	second, err := main.Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 91, Kind: personal.EventUser, Text: "报告呢？"}, Tasks: []personal.Task{task}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Text, "推荐了 C 方案") || len(repo.continued) != 0 || len(repo.created) != 0 || len(repo.revisedKeys) != 0 {
		t.Fatalf("follow-up did not reuse saved result: output=%+v continued=%v created=%v revised=%v", second, repo.continued, repo.created, repo.revisedKeys)
	}
	if toolResult := fixtureRequestText(t, fixture.Requests[3]); !strings.Contains(toolResult, task.Summary) {
		t.Fatalf("saved task result was not returned to the model: %s", toolResult)
	}
}

func fixtureRequestText(t *testing.T, request map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(request["input"])
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	var parts []string
	var collect func(any)
	collect = func(value any) {
		switch value := value.(type) {
		case string:
			parts = append(parts, value)
		case []any:
			for _, item := range value {
				collect(item)
			}
		case map[string]any:
			for _, item := range value {
				collect(item)
			}
		}
	}
	collect(value)
	return strings.Join(parts, "\n")
}

func newMainAgentWithWindow(t *testing.T, fixture *testkit.ResponsesFixture, repo Repository, window, output int64) *MainAgent {
	t.Helper()
	main := newMainAgent(t, fixture, repo)
	limits, err := inputbudget.NewLimits(window, output)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := inputbudget.NewAgenticModel(main.Model, inputbudget.ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	main.Model = bounded
	return main
}
