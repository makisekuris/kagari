package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestScheduleReceiptSurvivesModelFailureAfterRegistration(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_schedule", personal.ScheduleInput{Title: "晨读", Goal: "读一篇文章", Cron: "0 9 * * *", Timezone: "Asia/Shanghai"}, "register"),
		testkit.ResponseRefusal(),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		t.Fatal("committed timing receipt sent back to the model")
		return "", nil
	}
	id := testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "每天早上九点安排晨读"))
	if worked, err := r.turn(ctx); !worked || err != nil {
		t.Fatalf("turn = %t, %v", worked, err)
	}
	msg := testkit.AssistantMessage(t, s, id)
	if !strings.Contains(msg.Text, "计划 #1「晨读」已启用") || !strings.Contains(msg.Text, "09:00") || msg.ScheduleID != 1 {
		t.Fatalf("missing committed outcome: %+v", msg)
	}
	plans, err := s.Schedules(ctx, false, 0, 20)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %+v, %v", plans, err)
	}
	due, err := s.DispatchDueSchedule(ctx, *plans[0].NextRunAt)
	if err != nil || due == nil || due.Outcome != "enqueued" {
		t.Fatalf("authorized schedule dispatch = %+v, %v", due, err)
	}
}

func TestCountdownToolCommitAndRecovery(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	r.Timezone = "Asia/Shanghai"
	steps := []personal.ReminderStep{{AfterMinutes: 30, Text: "休息十分钟。"}, {AfterMinutes: 10, Text: "本轮结束。"}}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_reminder", personal.ReminderInput{Title: "番茄钟", Steps: steps}, "timer"),
		testkit.ResponseRefusal(),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	id := testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "现在开始一轮 30+10，到点发消息"))
	if worked, err := r.turn(ctx); !worked || err != nil {
		t.Fatalf("turn = %t, %v", worked, err)
	}
	msg := testkit.AssistantMessage(t, s, id)
	if !strings.Contains(msg.Text, "计时提醒 #1「番茄钟」已启用") || msg.ScheduleID != 0 {
		t.Fatalf("timer receipt = %+v", msg)
	}
	all, err := s.Reminders(ctx, true, 0, 20)
	if err != nil || len(all) != 1 || all[0].NextRunAt == nil || all[0].Repeat || !all[0].NextRunAt.Equal(all[0].StartAt.Add(30*time.Minute)) {
		t.Fatalf("timers = %+v, %v", all, err)
	}
	// Use the same query tool exposed to the owner. Timer IDs and cron IDs
	// are independent even when the numeric IDs happen to coincide.
	query := testkit.OpenUserEvent(t, s, 2, 2, "查一下定时提醒")
	f := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("list_schedules", map[string]any{}, "inventory"),
		testkit.ResponseText(`{"text":"有一个计时提醒。","reply_kind":"content","attachments":[]}`),
	}}
	out, err := testMainAgent(t, f, s).Think(ctx, personal.TurnInput{Event: query})
	if err != nil || len(f.Requests) != 2 || !strings.Contains(testkit.RequestInput(f.Requests[1]), "番茄钟") {
		t.Fatalf("inventory = %+v, %v", out, err)
	}
	testkit.FinishEvent(t, s, query, out, nil)
	for i, elapsed := range []time.Duration{30 * time.Minute, 40 * time.Minute} {
		if worked, err := s.DispatchDueReminder(ctx, all[0].StartAt.Add(elapsed)); !worked || err != nil {
			t.Fatalf("step %d = %t, %v", i, worked, err)
		}
		history, err := s.History(ctx, query.ID+1, 30)
		found := false
		for _, item := range history {
			if item.EventID == id && item.Text == steps[i].Text {
				found = true
			}
		}
		if err != nil || !found {
			t.Fatalf("step %d missing from outgoing history: %v", i, err)
		}
	}
	if worked, err := s.DispatchDueReminder(ctx, all[0].StartAt.Add(time.Hour)); worked || err != nil {
		t.Fatalf("finished timer fired again: %t, %v", worked, err)
	}
}

func TestDueReminderDeliversExactTextThroughRuntime(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	text := "起来走走。"
	start := time.Now().Add(-2 * time.Minute)
	input := personal.ReminderInput{Title: "活动提醒", StartAt: &start, Steps: []personal.ReminderStep{{AfterMinutes: 1, Text: text}}}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_reminder", input, "remind"),
		testkit.ResponseText(`{"text":"计时提醒已登记。","reply_kind":"content","attachments":[]}`),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "到点发一句：起来走走。"))
	if worked, err := r.turn(ctx); !worked || err != nil {
		t.Fatalf("turn = %t, %v", worked, err)
	}
	if worked, err := r.deliver(ctx); !worked || err != nil {
		t.Fatalf("registration receipt = %t, %v", worked, err)
	}
	if worked, err := r.schedule(ctx); !worked || err != nil {
		t.Fatalf("schedule = %t, %v", worked, err)
	}
	var sent []string
	send := r.Send
	r.Send = func(ctx context.Context, msg channel.Content) (channel.Receipt, error) {
		sent = append(sent, msg.Text)
		return send(ctx, msg)
	}
	if worked, err := r.deliver(ctx); !worked || err != nil {
		t.Fatalf("deliver = %t, %v", worked, err)
	}
	if worked, err := r.deliver(ctx); worked || err != nil || len(sent) != 1 || sent[0] != text {
		t.Fatalf("delivered messages = %q, further work = %t, %v", sent, worked, err)
	}
	tasks, err := s.Tasks(ctx, 20)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("fixed reminder created tasks = %+v, %v", tasks, err)
	}
}
