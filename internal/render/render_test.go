package render

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"kagari/internal/domain"
)

func TestChunksPreserveEmojiAndText(t *testing.T) {
	text := strings.Repeat("中文😀\n", 2000)
	chunks := Chunks(text)
	if strings.Join(chunks, "") != text {
		t.Fatal("text changed")
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > 3500 {
			t.Fatal("message exceeds limit")
		}
	}
}

func TestAnalysisHidesEmptyDiscussionAndLegacyRelevance(t *testing.T) {
	text := Analysis(domain.Result{Analysis: domain.Analysis{
		Title: "标题", Overview: "概述", Relevance: "legacy preference",
		Headings: &domain.AnalysisHeadings{Summary: "摘要", Discussion: "空讨论标题", Evaluation: "评价", Uncertainties: "限制", Sources: "来源"},
	}})
	if strings.Contains(text, "legacy preference") || strings.Contains(text, "关注点") || strings.Contains(text, "讨论者观点") || strings.Contains(text, "空讨论标题") {
		t.Fatalf("rendered legacy relevance or empty discussion: %s", text)
	}
}

func TestAnalysisUsesModelHeadings(t *testing.T) {
	text := Analysis(domain.Result{
		Analysis: domain.Analysis{
			Title: "标题", Overview: "概述",
			Headings: &domain.AnalysisHeadings{
				Summary: " 自定义摘要： ", Discussion: "自定义讨论", Evaluation: "自定义评价",
				Uncertainties: "自定义限制：", Sources: "自定义来源",
			},
			Summary:       []domain.Claim{{Text: "摘要结论", SourceIDs: []string{"s1"}}},
			Discussion:    []domain.Claim{{Text: "讨论观点"}},
			Evaluation:    []domain.Claim{{Text: "评价结论"}},
			Uncertainties: []string{"尚未确认"},
		},
		Sources: []domain.Source{{ID: "s1", Title: "来源标题", URL: "https://example.com", Status: "ok"}},
	})
	for _, want := range []string{
		"自定义摘要：\n• 摘要结论 [1]", "自定义讨论：\n• 讨论观点", "自定义评价：\n• 评价结论",
		"自定义限制：\n• 尚未确认", "自定义来源：\n[1] 来源标题", "https://example.com",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered output missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "自定义摘要：：") || strings.Contains(text, "AI 摘要") || strings.Contains(text, "Agent 评价") {
		t.Fatalf("model headings were replaced or duplicated: %s", text)
	}
}

func TestAnalysisRendersLegacyJSONWithoutHeadings(t *testing.T) {
	var result domain.Result
	err := json.Unmarshal([]byte(`{"analysis":{"title":"旧档案","overview":"旧概述","summary":[{"text":"旧结论","source_ids":["s1"]}],"discussion":[],"evaluation":[],"uncertainties":["旧限制"]},"sources":[{"id":"s1","title":"旧来源","url":"https://example.com","status":"ok"}]}`), &result)
	if err != nil {
		t.Fatal(err)
	}

	text := Analysis(result)
	for _, want := range []string{"AI 摘要：\n• 旧结论 [1]", "未确认与限制：\n• 旧限制", "原文与来源：\n[1] 旧来源", "https://example.com"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered output missing %q: %s", want, text)
		}
	}
}
