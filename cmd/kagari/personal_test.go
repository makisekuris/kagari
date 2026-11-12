package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/personal"
	"kagari/internal/personal/sqlite"
)

func TestPersonalInspectionDoesNotRecoverServiceWork(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "personal.db")
	cfg.Personal.TaskBudget = time.Minute
	s, err := sqlite.NewStore(cfg.Storage.Path, cfg.Personal.TaskBudget)
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
	ids, err := s.AcceptBatch(ctx, "telegram", "bot", "1", []personal.Input{{Channel: "telegram", Account: "bot", Sender: "owner", MessageID: "11", Text: "create a schedule"}})
	if err != nil {
		t.Fatal(err)
	}
	scheduleEventID := ids[0]
	if _, err = s.ClaimEvent(ctx); err != nil {
		t.Fatal(err)
	}
	schedule, _, err := s.CreateSchedule(ctx, scheduleEventID, "create-schedule", personal.ScheduleInput{
		Title: "Morning report", Goal: "Summarize the latest updates", Cron: "0 9 * * *", Timezone: "UTC", WindowMode: personal.WindowNone,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteEvent(ctx, scheduleEventID, personal.TurnOutput{}, nil); err != nil {
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
	for _, query := range []struct {
		name string
		run  func([]string, *bytes.Buffer) error
		args []string
	}{
		{name: "schedules", run: func(args []string, out *bytes.Buffer) error { return inspectSchedules(ctx, cfg, args, out) }, args: []string{"-limit", "100"}},
		{name: "schedule details", run: func(args []string, out *bytes.Buffer) error { return inspectSchedules(ctx, cfg, args, out) }, args: []string{strconv.FormatInt(schedule.ID, 10)}},
		{name: "schedule history", run: func(args []string, out *bytes.Buffer) error { return inspectScheduleHistory(ctx, cfg, args, out) }, args: []string{"-limit", "100", strconv.FormatInt(schedule.ID, 10)}},
	} {
		var out bytes.Buffer
		if err := query.run(query.args, &out); err != nil {
			t.Fatalf("%s: %v", query.name, err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("%s returned invalid JSON: %s", query.name, out.String())
		}
	}
	for _, query := range []struct {
		name string
		run  func([]string, *bytes.Buffer) error
		args []string
	}{
		{name: "bad schedule limit", run: func(args []string, out *bytes.Buffer) error { return inspectSchedules(ctx, cfg, args, out) }, args: []string{"-limit", "101"}},
		{name: "missing schedule ID", run: func(args []string, out *bytes.Buffer) error { return inspectSchedules(ctx, cfg, args, out) }, args: []string{"0"}},
		{name: "bad history limit", run: func(args []string, out *bytes.Buffer) error { return inspectScheduleHistory(ctx, cfg, args, out) }, args: []string{"-limit", "0", "1"}},
		{name: "missing history ID", run: func(args []string, out *bytes.Buffer) error { return inspectScheduleHistory(ctx, cfg, args, out) }, args: []string{"1", "2"}},
	} {
		if err := query.run(query.args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s accepted: %v", query.name, query.args)
		}
	}
	missingCfg := cfg
	missingCfg.Storage.Path = filepath.Join(t.TempDir(), "missing.db")
	if err := inspectSchedules(ctx, missingCfg, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("schedule inspection initialized a missing database")
	}
	if _, err := os.Stat(missingCfg.Storage.Path); !os.IsNotExist(err) {
		t.Fatalf("schedule inspection created a database: %v", err)
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
