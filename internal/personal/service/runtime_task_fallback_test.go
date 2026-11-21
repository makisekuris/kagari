package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeFailedTaskFallbackKeepsSourceReferenceWithoutExcerpt(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	r.Prepare = channel.PrepareText
	const sourceURL = "https://example.com/guide"
	const sourceText = "English source excerpt that must stay in saved evidence."
	sourceBody := strings.Repeat(sourceText, 80)
	var sourceID string
	r.Execute = func(ctx context.Context, run personal.Run, _ func(string) error) (personal.StageResult, error) {
		ref, err := s.SaveSource(ctx, run.ID, "guide", domain.Source{
			URL: sourceURL, Title: "Guide", Status: "ok", Content: sourceBody, FetchedAt: time.Now(),
		})
		if err != nil {
			return personal.StageResult{}, err
		}
		sourceID = ref.ID
		return personal.StageResult{
			Summary: "已保留的来源记录：\n- Guide — https://example.com/guide\n  读取完成",
			Sources: []domain.SourceRef{ref},
		}, errors.New("stage analysis was interrupted")
	}
	r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if input.Event.Kind == personal.EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "create", "阅读指南", personal.TaskInput{Goal: "Read the supplied guide."})
			return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		return personal.TurnOutput{}, errors.New("reply model unavailable")
	}
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/guide"))
	for _, step := range []func(context.Context) (bool, error){r.turn, r.task, r.turn} {
		if worked, err := step(ctx); err != nil || !worked {
			t.Fatalf("runtime step = %v, %v", worked, err)
		}
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil {
		t.Fatalf("failed task fallback = %+v, %v", message, err)
	}
	if !strings.Contains(message.Text, "这次未能完成") || !strings.Contains(message.Text, sourceURL) || strings.Count(message.Text, sourceURL) != 1 {
		t.Fatalf("fallback lost failure state or source identity, or duplicated its URL: %q", message.Text)
	}
	if strings.Contains(message.Text, sourceText) || strings.Contains(message.Text, "returned text:") {
		t.Fatalf("fallback copied source body into the chat reply: %q", message.Text)
	}
	if extra, err := s.ClaimDelivery(ctx); err != nil || extra != nil {
		t.Fatalf("short failure fallback split into extra messages: %+v, %v", extra, err)
	}
	storedSource, err := s.ExpandSource(ctx, sourceID)
	if err != nil || storedSource.Content != sourceBody {
		t.Fatalf("fallback dropped the full saved evidence: source=%+v err=%v", storedSource, err)
	}
}
