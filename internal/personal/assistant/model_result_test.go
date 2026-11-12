package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/textpage"
)

type modelResultRepo struct {
	*toolRepo
	refs map[string]domain.SourceRef
}

func (r *modelResultRepo) SaveSource(ctx context.Context, runID int64, key string, source domain.Source) (domain.SourceRef, error) {
	ref, err := r.toolRepo.SaveSource(ctx, runID, key, source)
	if err == nil {
		if r.refs == nil {
			r.refs = map[string]domain.SourceRef{}
		}
		r.refs[ref.ID] = ref
	}
	return ref, err
}

func (r *modelResultRepo) ReadMaterial(_ context.Context, _ personal.ReadScope, id, part string, request textpage.Request) (personal.MaterialText, error) {
	if part != "" && part != "body" {
		return personal.MaterialText{}, errors.New("part must be body")
	}
	source, ok := r.sources[id]
	if !ok {
		return personal.MaterialText{}, errors.New("source not found")
	}
	ref := r.refs[id]
	page, err := textpage.Read(source.Content, request, false)
	return personal.MaterialText{Material: personal.Material{ID: id, RunID: ref.RunID, Kind: "source", Version: page.Version, CollectedAt: ref.FetchedAt, Source: &ref}, Page: page}, err
}

func TestThinkBoundsTaskCallbackAndKeepsFullRunAvailable(t *testing.T) {
	const fullText = "FULL_SOURCE_SENTINEL"
	repo := &modelResultRepo{toolRepo: newToolRepo()}
	source := domain.Source{ID: "source-1", URL: "https://example.com/report", Title: "Report source", Status: "ok", Content: strings.Repeat(fullText, 300), FetchedAt: time.Now()}
	ref, err := repo.SaveSource(context.Background(), 31, "fixture-report", source)
	if err != nil {
		t.Fatal(err)
	}
	result := personal.StageResult{
		Summary: "Report saved.",
		Sources: []domain.SourceRef{ref},
		Files:   []string{"reports/report.md"},
	}
	fullResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	currentRun := personal.Run{ID: 31, TaskID: 7, Kind: personal.RunTask, Status: personal.RunCompleted, Result: string(fullResult)}
	repo.tasks[7] = personal.Task{ID: 7, Title: "Write report", Goal: "Save the report", Status: personal.TaskAwaitingReview}
	repo.runs[currentRun.ID] = currentRun
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_run", map[string]any{"run_id": currentRun.ID}, "get-full-run"),
		testkit.ResponseTool("read_material", map[string]any{"material_id": ref.ID}, "read-source"),
		testkit.ResponseText(`{"text":"Saved the report at reports/report.md.","reply_kind":"content","attachments":[]}`),
	}}
	_, err = newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 90, Kind: personal.EventTask, TaskID: 7, RunID: currentRun.ID},
		Tasks: []personal.Task{repo.tasks[7]}, CurrentRun: &currentRun,
	})
	if err != nil {
		t.Fatal(err)
	}

	var sent personal.TurnInput
	inputMessages, ok := fixture.Requests[0]["input"].([]any)
	if !ok {
		t.Fatalf("callback input = %#v", fixture.Requests[0]["input"])
	}
	for _, item := range inputMessages {
		message, _ := item.(map[string]any)
		if message["role"] == "user" {
			content, _ := message["content"].(string)
			if err := json.Unmarshal([]byte(content), &sent); err != nil {
				t.Fatal(err)
			}
		}
	}
	if sent.CurrentRun == nil {
		t.Fatalf("callback input had no current run: %#v", fixture.Requests[0]["input"])
	}
	if strings.Contains(sent.CurrentRun.Result, fullText) {
		t.Fatal("callback eagerly included the stored source body")
	}
	var bounded personal.StageResult
	if err := json.Unmarshal([]byte(sent.CurrentRun.Result), &bounded); err != nil {
		t.Fatalf("bounded callback result is invalid JSON: %v", err)
	}
	if bounded.Summary != "Report saved." || len(bounded.Files) != 1 || bounded.Files[0] != "reports/report.md" {
		t.Fatalf("bounded callback result lost summary or file reference: %+v", bounded)
	}
	if len(bounded.Sources) != 1 || bounded.Sources[0].ID != ref.ID || bounded.Sources[0].URL != "https://example.com/report" || bounded.Sources[0].Title != "Report source" || bounded.Sources[0].Status != "ok" {
		t.Fatalf("bounded callback result lost source metadata or retained its body: %+v", bounded.Sources)
	}
	if currentRun.Result != string(fullResult) || repo.runs[currentRun.ID].Result != string(fullResult) {
		t.Fatal("bounding the callback mutated the original run result")
	}
	if len(fixture.Requests) < 3 || !strings.Contains(testkit.RequestInput(fixture.Requests[2]), fullText) {
		t.Fatal("read_material did not make the stored source text available to the model")
	}
	if got := fixtureSource(t, repo.toolRepo, ref); got.Content != source.Content {
		t.Fatal("persisted source body changed")
	}
}

func TestTaskOutcomeArtifactsDoNotRequireNotification(t *testing.T) {
	results := map[string]personal.StageResult{
		"sources":     {Sources: []domain.SourceRef{{ID: "source-1"}}},
		"files":       {Files: []string{"reports/final.md"}},
		"attachments": {Attachments: []channel.Attachment{{ID: "attachment-1"}}},
	}
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			input := personal.TurnInput{
				Event:      personal.Event{Kind: personal.EventTask, TaskID: 7},
				Tasks:      []personal.Task{{ID: 7, Status: personal.TaskAwaitingReview}},
				CurrentRun: &personal.Run{TaskID: 7, Result: string(raw)},
			}
			if taskOutcomeRequiresNotification(input) {
				t.Fatal("task result artifacts alone required notification")
			}
		})
	}
}
