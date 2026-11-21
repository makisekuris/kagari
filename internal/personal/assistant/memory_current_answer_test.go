package assistant

import (
	"context"
	"testing"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

const currentMemoryNoMatchReply = "当前没有查到匹配的有效记忆。"

func thinkCurrentMemoryAnswer(t *testing.T, repo Repository, responses ...testkit.Response) (personal.TurnOutput, *testkit.ResponsesFixture) {
	t.Helper()
	fixture := &testkit.ResponsesFixture{T: t, Responses: responses}
	input := personal.TurnInput{
		Event:   personal.Event{ID: 711, Kind: personal.EventUser, Text: "按现在有效的长期记忆，我的测试笔记本叫什么？"},
		History: []personal.Message{{EventID: 701, Role: "user", Text: "我的测试笔记本叫白桦。"}, {EventID: 702, Role: "assistant", Text: "已忘记测试笔记本名称。"}},
	}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return output, fixture
}

func TestCurrentMemoryAnswerMissOverridesLeakingModelText(t *testing.T) {
	repo := newToolRepo()
	repo.memories["test_laptop_name"] = personal.Memory{Key: "test_laptop_name", Version: 3, Deleted: true}
	output, fixture := thinkCurrentMemoryAnswer(t, repo,
		testkit.ResponseTool(memoryCurrentAnswerToolName, memoryCurrentAnswerArgs{Key: "missing_key"}, "current-memory"),
		testkit.ResponseText(`{"text":"你的测试笔记本以前叫白桦。","reply_kind":"content","attachments":[]}`),
	)
	if output.Text != currentMemoryNoMatchReply || output.Silent || output.ReplyKind != personal.ReplyContent || !output.SkipReplyPresentation {
		t.Fatalf("empty current-memory result did not replace free text with a fixed reply: %+v", output)
	}
	if len(fixture.Requests) != 2 {
		t.Fatalf("model requests=%d, want tool call and final reply", len(fixture.Requests))
	}
}

func TestCurrentMemoryAnswerUsesExactKeyBeforeRelatedKeywords(t *testing.T) {
	repo := newToolRepo()
	repo.memories["test_laptop_name"] = personal.Memory{Key: "test_laptop_name", Version: 3, Deleted: true}
	repo.memories["test_laptop_model"] = personal.Memory{Key: "test_laptop_model", Value: "Notebook Pro", Version: 1}
	output, _ := thinkCurrentMemoryAnswer(t, repo,
		testkit.ResponseTool(memoryCurrentAnswerToolName, memoryCurrentAnswerArgs{Key: "test_laptop_name", Query: "laptop"}, "current-memory"),
		testkit.ResponseText(`{"text":"你的笔记本型号是 Notebook Pro。","reply_kind":"content","attachments":[]}`),
	)
	if output.Text != currentMemoryNoMatchReply || !output.SkipReplyPresentation {
		t.Fatalf("a tombstoned exact key fell through to an unrelated keyword match: %+v", output)
	}
}

func TestCurrentMemoryAnswerHitAndLaterHitWin(t *testing.T) {
	repo := newToolRepo()
	repo.memories["test_laptop_name"] = personal.Memory{Key: "test_laptop_name", Value: "白桦", Version: 2}
	want := "你的测试笔记本叫白桦。"
	output, _ := thinkCurrentMemoryAnswer(t, repo,
		testkit.ResponseTool(memoryCurrentAnswerToolName, memoryCurrentAnswerArgs{Key: "missing_key"}, "first-empty"),
		testkit.ResponseTool(memoryCurrentAnswerToolName, memoryCurrentAnswerArgs{Key: "test_laptop_name"}, "second-hit"),
		testkit.ResponseText(`{"text":"`+want+`","reply_kind":"content","attachments":[]}`),
	)
	if output.Text != want || output.SkipReplyPresentation {
		t.Fatalf("a later matching current-memory result was hidden by an earlier miss: %+v", output)
	}
}

func TestCurrentMemoryAnswerDoesNotOverrideContextRead(t *testing.T) {
	repo := newToolRepo()
	const want = "继续处理较大的请求。"
	output, _ := thinkCurrentMemoryAnswer(t, repo,
		testkit.ResponseTool(memoryCurrentAnswerToolName, memoryCurrentAnswerArgs{Key: "missing_key"}, "current-memory"),
		testkit.ResponseTool("memory_search", memorySearchArgs{Query: "laptop"}, "context-memory"),
		testkit.ResponseText(`{"text":"`+want+`","reply_kind":"content","attachments":[]}`),
	)
	if output.Text != want || output.SkipReplyPresentation {
		t.Fatalf("current-memory miss swallowed a separate context read: %+v", output)
	}
}
