package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/personal"
)

func TestPersonalInspectionDoesNotRecoverServiceWork(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "personal.db")
	cfg.Personal.TaskBudget = time.Minute
	s, err := personal.NewStore(cfg.Storage.Path, cfg.Personal.TaskBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := acceptTestInput(ctx, s, "research")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimEvent(ctx); err != nil {
		t.Fatal(err)
	}
	task, run, err := s.CreateTask(ctx, id, "create", "research", personal.TaskInput{Goal: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteEvent(ctx, id, personal.TurnOutput{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimRun(ctx, personal.RunTask); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"tasks"}, {"tasks", "1"}, {"runs", "1"}, {"memories"}, {"memory-history"}} {
		var out bytes.Buffer
		if err := inspectPersonal(ctx, cfg, args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("%v returned invalid JSON: %s", args, out.String())
		}
	}
	got, err := s.Run(ctx, run.ID)
	if err != nil || got.Status != personal.RunRunning {
		t.Fatalf("inspection recovered run: %+v, %v", got, err)
	}
	current, err := s.Task(ctx, task.ID)
	if err != nil || current.Status != personal.TaskRunning {
		t.Fatalf("inspection changed task: %+v, %v", current, err)
	}
	for _, args := range [][]string{{"runs"}, {"tasks", "0"}, {"tasks", "999"}, {"runs", "nope"}, {"memories", "a", "b"}} {
		if err := inspectPersonal(ctx, cfg, args, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
}
