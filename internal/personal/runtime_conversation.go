package personal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kagari/internal/logging"

	"go.uber.org/zap"
)

func (r *Runtime) turn(ctx context.Context) (worked bool, turnErr error) {
	event, err := r.Store.ClaimEvent(ctx)
	if err != nil || event == nil {
		return false, err
	}
	log := r.eventLog(*event)
	ctx = logging.WithLogger(ctx, log)
	turnStarted := time.Now()
	outcome := "completed"
	log.Info("conversation.started", zap.Any("payload", event))
	defer func() {
		fields := []zap.Field{zap.Int64("elapsed_ms", time.Since(turnStarted).Milliseconds())}
		if turnErr != nil {
			log.Warn("conversation.failed", append(fields, logging.ErrorFields(turnErr)...)...)
		} else {
			log.Info("conversation.finished", append(fields, zap.String("outcome", outcome))...)
		}
	}()
	input, err := r.context(ctx, *event)
	if err != nil {
		return true, err
	}
	var task *Task
	if event.Kind == EventTask {
		task, err = r.Store.Task(ctx, event.TaskID)
		if err != nil {
			return true, err
		}
		if input.CurrentRun == nil || task == nil || input.CurrentRun.Revision != task.Revision || task.Status == TaskCancelled {
			outcome = "stale"
			return true, r.Store.CompleteEvent(ctx, event.ID, TurnOutput{}, nil)
		}
		if task.Status == TaskWaitingBudget {
			return true, r.publish(ctx, *event, TurnOutput{TaskID: task.ID, Text: fmt.Sprintf("任务 #%d「%s」已用完本轮执行预算，等待你选择是否继续。\n\n%s", task.ID, task.Title, clip(task.Summary, 6000))})
		}
	}
	if event.Interrupted {
		outcome = "recovered"
		return true, r.recoverTurn(ctx, *event, task)
	}
	started := time.Now()
	turnCtx, cancel := context.WithCancel(ctx)
	if task != nil {
		cancel()
		remaining := time.Duration(task.BudgetMS-task.UsedMS) * time.Millisecond
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		turnCtx, cancel = context.WithTimeout(ctx, remaining)
	}
	defer cancel()
	accountDone := make(chan error, 1)
	if task != nil {
		go func() {
			for wait(turnCtx, r.idleDelay) {
				if err := r.Store.AccountEvent(turnCtx, event.ID, time.Since(started)); err != nil {
					if turnCtx.Err() == nil {
						cancel()
						accountDone <- err
						return
					}
					break
				}
			}
			accountDone <- nil
		}()
	}
	output, thinkErr := r.Think(turnCtx, input)
	cancel()
	if task != nil {
		if err := <-accountDone; err != nil {
			return true, err
		}
		persistCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = r.Store.AccountEvent(persistCtx, event.ID, time.Since(started))
		stop()
		if err != nil {
			return true, err
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if thinkErr != nil || (!output.Silent && strings.TrimSpace(output.Text) == "" && len(output.Attachments) == 0) || (event.Kind == EventUser && output.Silent) {
		outcome = "fallback_reply"
		log.Warn("conversation.model.failed", logging.ErrorFields(thinkErr)...)
		fallback := "这轮回复未能完成。已提交的任务和记忆操作仍保留，可以继续查询。"
		if errors.Is(thinkErr, ErrAttachmentUnavailable) {
			fallback = "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"
		}
		output = TurnOutput{Text: fallback, TaskID: event.TaskID}
		if event.Kind == EventTask {
			output.Text = fmt.Sprintf("后台结果（执行 #%d）：\n%s", event.RunID, clip(event.Text, 6000))
		}
	}
	if task != nil {
		current, err := r.Store.Task(ctx, task.ID)
		if err != nil {
			return true, err
		}
		if current != nil && current.Status == TaskAwaitingReview {
			// An incomplete planning turn must not strand work without a visible state.
			_, err = r.Store.SettleTask(ctx, event.ID, task.ID, current.Revision, TaskWaitingInput, current.Summary)
			if err != nil {
				return true, err
			}
			output = TurnOutput{TaskID: task.ID, Text: fmt.Sprintf("任务 #%d 已保存阶段成果，尚未确定下一步。请补充要求或让我继续。\n\n%s", task.ID, clip(current.Summary, 6000))}
		} else if current != nil && current.Status == TaskWaitingBudget {
			output = TurnOutput{TaskID: task.ID, Text: fmt.Sprintf("任务 #%d 已用完本轮执行预算，等待你选择是否继续。\n\n%s", task.ID, clip(current.Summary, 6000))}
		}
	}
	if errors.Is(thinkErr, ErrAttachmentUnavailable) && !strings.Contains(output.Text, "要发送的附件已删除或不可用") {
		if output.Text != "" {
			output.Text += "\n\n"
		}
		output.Text += "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"
		output.Attachments = nil
	}
	if output.Silent {
		outcome = "silent"
	}
	return true, r.publish(ctx, *event, output)
}

// A restarted model could paraphrase an already committed tool call and create a second task.
func (r *Runtime) recoverTurn(ctx context.Context, event Event, task *Task) error {
	output := TurnOutput{Text: "处理上一条消息时服务中断了。已提交的任务和记忆操作仍保留，可以查询；尚未提交的部分请重新告诉我。"}
	if task != nil {
		if task.Status == TaskQueued {
			return r.publish(ctx, event, TurnOutput{TaskID: task.ID, Silent: true})
		}
		if task.Status == TaskAwaitingReview {
			if _, err := r.Store.SettleTask(ctx, event.ID, task.ID, task.Revision, TaskWaitingInput, task.Summary); err != nil {
				return err
			}
			output.Text = fmt.Sprintf("任务 #%d 已保存阶段成果，但后续规划被中断。请告诉我是否继续。\n\n%s", task.ID, clip(task.Summary, 6000))
		} else {
			output.Text = fmt.Sprintf("任务 #%d 的状态已保存（%s），此前的回复被中断。\n\n%s", task.ID, task.Status, clip(task.Summary, 6000))
		}
		output.TaskID = task.ID
	}
	return r.publish(ctx, event, output)
}

func (r *Runtime) publish(ctx context.Context, event Event, output TurnOutput) error {
	if output.Silent {
		return r.Store.CompleteEvent(ctx, event.ID, TurnOutput{TaskID: output.TaskID, Silent: true}, nil)
	}
	var chunks []string
	if output.Text != "" {
		var err error
		chunks, err = r.Prepare(output.Text)
		if err != nil {
			return err
		}
	}
	if len(chunks) == 0 && len(output.Attachments) == 0 {
		return errors.New("personal delivery produced no message")
	}
	if err := r.Store.CompleteEvent(ctx, event.ID, output, chunks); err != nil {
		if !errors.Is(err, ErrAttachmentUnavailable) || len(output.Attachments) == 0 {
			return err
		}
		fallback := TurnOutput{TaskID: output.TaskID, Text: "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"}
		fallbackChunks, prepareErr := r.Prepare(fallback.Text)
		if prepareErr != nil {
			return prepareErr
		}
		return r.Store.CompleteEvent(ctx, event.ID, fallback, fallbackChunks)
	}
	return nil
}

func (r *Runtime) context(ctx context.Context, event Event) (TurnInput, error) {
	input := TurnInput{Event: event}
	var err error
	input.History, err = r.Store.History(ctx, event.ID, 20)
	if err != nil {
		return input, err
	}
	input.Tasks, err = r.Store.Tasks(ctx, 10)
	if err != nil {
		return input, err
	}
	if event.RunID != 0 {
		input.CurrentRun, err = r.Store.Run(ctx, event.RunID)
		if err != nil {
			return input, err
		}
	}
	if event.TaskID != 0 {
		found := false
		for _, task := range input.Tasks {
			if task.ID == event.TaskID {
				found = true
			}
		}
		if !found {
			task, err := r.Store.Task(ctx, event.TaskID)
			if err != nil {
				return input, err
			}
			if task != nil {
				input.Tasks = append([]Task{*task}, input.Tasks...)
			}
		}
	}
	input.Memories, err = r.Store.Memories(ctx, "", 20)
	if err != nil {
		return input, err
	}
	for i := range input.History {
		input.History[i].Text = clip(input.History[i].Text, 2000)
	}
	for i := range input.Tasks {
		input.Tasks[i].Goal = clip(input.Tasks[i].Goal, 2000)
		input.Tasks[i].Summary = clip(input.Tasks[i].Summary, 3000)
		input.Tasks[i].Context = clip(input.Tasks[i].Context, 2000)
	}
	for i := range input.Memories {
		input.Memories[i].Value = clip(input.Memories[i].Value, 800)
	}
	return input, nil
}
