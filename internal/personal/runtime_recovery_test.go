package personal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestRuntimeRecoveredUserTurnPreservesActionsWithoutReplayingTools(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "recover.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "research this")
	task, first := createTask(t, s, event, "first", "research", "research this", "")
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(context.Context, TurnInput) (TurnOutput, error) {
		t.Error("interrupted turn was replanned")
		return TurnOutput{}, nil
	}
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx, 10)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("recovery duplicated work: %+v, %v", tasks, err)
	}
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil || run.ID != first.ID {
		t.Fatalf("recovery did not release original task: %+v, %v", run, err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || !strings.Contains(message.Text, "中断") {
		t.Fatalf("recovery did not explain interruption: %+v, %v", message, err)
	}
}

func TestRuntimeRecoveredCallbackReleasesOnlyItsCommittedContinuation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "callback.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "research")
	task, first := createTask(t, s, event, "create", "research", "research", "")
	finishEvent(t, s, event, TurnOutput{}, nil)
	if _, err := s.ClaimRun(ctx, RunTask); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, first.ID, StageResult{Summary: "first result"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := s.ContinueTask(ctx, callback.ID, "continue", task.ID, task.Revision, "finish", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(context.Context, TurnInput) (TurnOutput, error) {
		t.Error("callback replanned")
		return TurnOutput{}, nil
	}
	if _, err = r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimRun(ctx, RunTask)
	if err != nil || claimed == nil || claimed.ID != next.ID {
		t.Fatalf("continuation lost: %+v, %v", claimed, err)
	}
	if message, err := s.ClaimDelivery(ctx); err != nil || message != nil {
		t.Fatalf("ordinary recovered continuation notified: %+v, %v", message, err)
	}
}

func TestLongStageEvidenceRemainsQueryableAndCanContinue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "checkpoint.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "research")
	task, first := createTask(t, s, event, "create", "research", "research", "")
	finishEvent(t, s, event, TurnOutput{}, nil)
	if _, err := s.ClaimRun(ctx, RunTask); err != nil {
		t.Fatal(err)
	}
	result := StageResult{Summary: strings.Repeat("summary ", 4000), Sources: []domain.Source{{ID: "s1", URL: "https://example.com/source", Status: "ok", Content: strings.Repeat("evidence", 6000)}}}
	if err := s.FinishStage(ctx, first.ID, result, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := s.ContinueTask(ctx, callback.ID, "next", task.ID, task.Revision, "check the next source", false)
	if err != nil {
		t.Fatal(err)
	}
	var input TaskInput
	if err = json.Unmarshal([]byte(next.Input), &input); err != nil {
		t.Fatal(err)
	}
	if len([]rune(input.Previous)) > maxTaskChars || !json.Valid([]byte(input.Previous)) || !strings.Contains(input.Previous, "https://example.com/source") {
		t.Fatal("invalid bounded checkpoint")
	}
	stored, err := s.Run(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	var full StageResult
	if err = json.Unmarshal([]byte(stored.Result), &full); err != nil || len(full.Sources) != 1 || full.Sources[0].Content != result.Sources[0].Content {
		t.Fatal("full stage evidence was lost")
	}
}
