package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"go.uber.org/zap"

	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/personal"
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
	intents, err := r.Store.EventDeliveries(ctx, event.ID)
	if err != nil {
		return true, err
	}
	if len(intents) > 0 {
		outcome = "recovered_delivery"
		output := personal.TurnOutput{}
		if event.Kind == personal.EventUser && event.Interrupted {
			output, err = r.committedActionSupplement(ctx, *event, output)
			if err != nil {
				return true, err
			}
		}
		return true, r.publishIntents(ctx, *event, intents, output)
	}
	if event.Kind == personal.EventTask {
		state, err := r.taskCallbackState(ctx, *event)
		if err != nil {
			return true, err
		}
		switch state {
		case personal.TaskCallbackStale:
			outcome = "stale"
			return true, r.completeStaleTaskCallback(ctx, *event)
		case personal.TaskCallbackContinued:
			// The callback already committed its child Run. Recover that exact
			// handoff without assembling context or asking the model again.
			task, err := r.Store.Task(ctx, event.TaskID)
			if err != nil {
				return true, err
			}
			if task == nil {
				return true, fmt.Errorf("personal: continued callback task %d is missing", event.TaskID)
			}
			if task.Status == personal.TaskWaitingBudget {
				sources, err := r.taskSourcesForEvent(ctx, *event)
				if err != nil {
					return true, err
				}
				return true, r.publish(ctx, *event, personal.TurnOutput{TaskID: task.ID, Text: taskBudgetText(task, sources), Purpose: personal.DeliveryStatus})
			}
			outcome = "continued"
			return true, r.Store.CompleteEvent(ctx, event.ID, personal.TurnOutput{TaskID: event.TaskID, Silent: true}, nil)
		}
	}
	input, err := r.context(ctx, *event)
	if err != nil {
		return true, err
	}
	var task *personal.Task
	if event.Kind == personal.EventTask {
		task, err = r.Store.Task(ctx, event.TaskID)
		if err != nil {
			return true, err
		}
		if input.CurrentRun == nil || task == nil || input.CurrentRun.Revision != task.Revision || task.Status == personal.TaskCancelled {
			outcome = "stale"
			return true, r.completeStaleTaskCallback(ctx, *event)
		}
		if task.Status == personal.TaskWaitingBudget {
			return true, r.publish(ctx, *event, personal.TurnOutput{TaskID: task.ID, Text: taskBudgetText(task, personal.SummarizeRunResult(*input.CurrentRun).Sources), Purpose: personal.DeliveryStatus})
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
	if event.Kind == personal.EventUser {
		input.OnInterimReply = func(ctx context.Context, text string) error {
			return r.queueInterimReply(ctx, *event, text)
		}
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
	intents, err = r.Store.EventDeliveries(ctx, event.ID)
	if err != nil {
		return true, err
	}
	if len(intents) > 0 {
		if output.CommittedActionsNeedReceipt {
			output, err = r.committedActionSupplement(ctx, *event, output)
			if err != nil {
				return true, err
			}
		}
		return true, r.publishIntents(ctx, *event, intents, output)
	}
	callbackState := personal.TaskCallbackCurrent
	if event.Kind == personal.EventTask {
		callbackState, err = r.taskCallbackState(ctx, *event)
		if err != nil {
			return true, err
		}
		if callbackState == personal.TaskCallbackStale {
			outcome = "stale"
			return true, r.completeStaleTaskCallback(ctx, *event)
		}
	}
	if thinkErr == nil && event.Kind == personal.EventUser && output.Silent {
		deferred, err := r.Store.CanDeferReply(ctx, event.ID)
		if err != nil {
			return true, err
		}
		if !deferred {
			if strings.TrimSpace(output.Text) != "" {
				output.Silent = false
			} else {
				thinkErr = errors.New("silent user response has no committed continuation")
			}
		}
	}
	if thinkErr != nil || (!output.Silent && strings.TrimSpace(output.Text) == "" && len(output.Attachments) == 0) {
		outcome = "fallback_reply"
		log.Warn("conversation.model.failed", logging.ErrorFields(thinkErr)...)
		attachmentUnavailable := errors.Is(thinkErr, personal.ErrAttachmentUnavailable)
		fallback := "这轮回复未能完成。已提交的任务、计划和记忆操作仍保留，可以继续查询。"
		var formatMessage string
		var skipPresentation bool
		if attachmentUnavailable {
			fallback = "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"
		} else if event.Kind == personal.EventUser {
			formatMessage = personal.ReplyFormatMessage(thinkErr)
			skipPresentation = true
			switch {
			case isModelQuotaFailure(thinkErr):
				fallback = "模型服务未能为这次请求分配额度。"
			case errors.Is(thinkErr, context.DeadlineExceeded):
				fallback = "模型响应超时，本次未能生成回复。"
			case formatMessage != "":
				fallback = formatMessage
			default:
				skipPresentation = false
			}
		}
		output = personal.TurnOutput{
			Text: fallback, TaskID: event.TaskID, ScheduleID: event.ScheduleID,
			SkipReplyPresentation: skipPresentation,
		}
		if !attachmentUnavailable {
			output, err = r.committedActionFallback(ctx, *event, output, formatMessage)
			if err != nil {
				return true, err
			}
		}
		if event.Kind == personal.EventTask {
			output.Text = fmt.Sprintf("「%s」的阶段结果：\n%s", task.Title, boundTaskSummary(event.Text, maxTaskFallbackSummaryBytes))
		}
	}
	if task != nil {
		current, err := r.Store.Task(ctx, task.ID)
		if err != nil {
			return true, err
		}
		if current == nil {
			return true, fmt.Errorf("personal: task %d disappeared during callback", task.ID)
		}
		if callbackState == personal.TaskCallbackContinued {
			// This callback may finish its own handoff, but the child Run now
			// owns any further Task decision.
			if current.Status == personal.TaskWaitingBudget {
				output.Text = taskBudgetText(current, personal.SummarizeRunResult(*input.CurrentRun).Sources)
				output.Purpose = personal.DeliveryStatus
				output.TaskID = task.ID
				output.Silent = false
			}
		} else if current.Status == personal.TaskAwaitingReview {
			pending, err := r.Store.PendingTaskInputs(ctx, task.ID)
			if err != nil {
				return true, err
			}
			if len(pending) > 0 {
				_, _, err = r.Store.ContinueTask(ctx, event.ID, "pending-inputs", task.ID, current.Revision, "Read the owner's additional material within the existing goal; build on the previous findings and report additions or corrections.", false)
				if errors.Is(err, personal.ErrStaleTaskCallback) {
					outcome = "stale"
					return true, r.completeStaleTaskCallback(ctx, *event)
				}
				if err != nil {
					return true, err
				}
				output = personal.TurnOutput{TaskID: task.ID, Silent: true}
			} else {
				// An incomplete planning turn must not strand work without a visible state.
				draft := fmt.Sprintf("「%s」的阶段成果已保存，后续安排未能完成。你可以让我继续。\n\n%s", current.Title, taskReplySummary(current))
				draft = appendTaskSources(draft, personal.SummarizeRunResult(*input.CurrentRun).Sources)
				_, err := r.Store.SettleTask(ctx, event.ID, task.ID, current.Revision, personal.TaskSettlement{
					Status: personal.TaskWaitingInput, Summary: current.Summary, Delivery: personal.DeliveryDraft{Text: draft},
				})
				if errors.Is(err, personal.ErrStaleTaskCallback) {
					outcome = "stale"
					return true, r.completeStaleTaskCallback(ctx, *event)
				}
				if err != nil {
					return true, err
				}
				output.Text = draft
				output.TaskID = task.ID
				output.Silent = false
			}
		} else if current.Status == personal.TaskWaitingBudget {
			output.Text = taskBudgetText(current, personal.SummarizeRunResult(*input.CurrentRun).Sources)
			output.Purpose = personal.DeliveryStatus
			output.TaskID = task.ID
			output.Silent = false
		} else if current.Status == personal.TaskCompleted || current.Status == personal.TaskFailed || current.Status == personal.TaskWaitingInput {
			output.Text = taskClosingText(current, personal.SummarizeRunResult(*input.CurrentRun).Sources)
			output.TaskID = task.ID
			output.Silent = false
		}
	}
	if errors.Is(thinkErr, personal.ErrAttachmentUnavailable) && !strings.Contains(output.Text, "要发送的附件已删除或不可用") {
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

func isModelQuotaFailure(err error) bool {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	switch apiErr.Code {
	case "pre_consume_token_quota_failed", "insufficient_quota":
		return true
	default:
		return false
	}
}

func (r *Runtime) taskCallbackState(ctx context.Context, event personal.Event) (personal.TaskCallbackState, error) {
	state, err := r.Store.TaskCallbackState(ctx, event.ID)
	if err != nil {
		return "", err
	}
	switch state {
	case personal.TaskCallbackCurrent, personal.TaskCallbackContinued, personal.TaskCallbackStale:
		return state, nil
	default:
		return "", fmt.Errorf("personal: event %d has unknown task callback state %q", event.ID, state)
	}
}

func (r *Runtime) completeStaleTaskCallback(ctx context.Context, event personal.Event) error {
	return r.Store.CompleteEvent(ctx, event.ID, personal.TurnOutput{Silent: true}, nil)
}

// A restarted model could paraphrase an already committed tool call and create a second task.
func (r *Runtime) recoverTurn(ctx context.Context, event personal.Event, task *personal.Task) error {
	output := personal.TurnOutput{ScheduleID: event.ScheduleID, Text: "处理上一条消息时服务中断了。已提交的任务、计划和记忆操作仍保留，可以查询；尚未提交的部分请重新告诉我。"}
	if event.Kind == personal.EventUser {
		recovered, err := r.committedActionFallback(ctx, event, output, "")
		if err != nil {
			return err
		}
		return r.publish(ctx, event, recovered)
	}
	if task != nil {
		if task.Status == personal.TaskQueued {
			return r.publish(ctx, event, personal.TurnOutput{TaskID: task.ID, ScheduleID: event.ScheduleID, Silent: true})
		}
		sources, err := r.taskSourcesForEvent(ctx, event)
		if err != nil {
			return err
		}
		if task.Status == personal.TaskAwaitingReview {
			draft := fmt.Sprintf("「%s」的阶段成果已保存，但后续安排被中断。请告诉我是否继续。\n\n%s", task.Title, taskReplySummary(task))
			draft = appendTaskSources(draft, sources)
			if _, err := r.Store.SettleTask(ctx, event.ID, task.ID, task.Revision, personal.TaskSettlement{
				Status: personal.TaskWaitingInput, Summary: task.Summary, Delivery: personal.DeliveryDraft{Text: draft},
			}); err != nil {
				if errors.Is(err, personal.ErrStaleTaskCallback) {
					return r.completeStaleTaskCallback(ctx, event)
				}
				return err
			}
			output.Text = draft
		} else if task.Status == personal.TaskWaitingBudget {
			output.Text = taskBudgetText(task, sources)
		} else if task.Status == personal.TaskCompleted || task.Status == personal.TaskFailed || task.Status == personal.TaskWaitingInput {
			output.Text = taskClosingText(task, sources)
		} else {
			output.Text = fmt.Sprintf("「%s」此前的回复被中断，已保存的结果如下。\n\n%s", task.Title, taskReplySummary(task))
			output.Text = appendTaskSources(output.Text, sources)
		}
		output.TaskID = task.ID
	}
	if event.Kind == personal.EventTask {
		state, err := r.taskCallbackState(ctx, event)
		if err != nil {
			return err
		}
		if state == personal.TaskCallbackStale {
			return r.completeStaleTaskCallback(ctx, event)
		}
	}
	return r.publish(ctx, event, output)
}

func (r *Runtime) taskSourcesForEvent(ctx context.Context, event personal.Event) ([]domain.SourceRef, error) {
	if event.RunID <= 0 {
		return nil, nil
	}
	run, err := r.Store.Run(ctx, event.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, nil
	}
	return personal.SummarizeRunResult(*run).Sources, nil
}

func (r *Runtime) publish(ctx context.Context, event personal.Event, output personal.TurnOutput) error {
	intents, err := r.Store.EventDeliveries(ctx, event.ID)
	if err != nil {
		return err
	}
	if len(intents) > 0 {
		return r.publishIntents(ctx, event, intents, output)
	}
	if event.Kind == personal.EventTask {
		state, err := r.taskCallbackState(ctx, event)
		if err != nil {
			return err
		}
		if state == personal.TaskCallbackStale {
			return r.completeStaleTaskCallback(ctx, event)
		}
	}
	if output.Silent {
		return r.Store.CompleteEvent(ctx, event.ID, personal.TurnOutput{TaskID: output.TaskID, ScheduleID: output.ScheduleID, Silent: true}, nil)
	}
	output, chunks, err := r.renderReply(ctx, event, output)
	if err != nil {
		return err
	}
	if err := r.Store.CompleteEvent(ctx, event.ID, output, chunks); err != nil {
		if !errors.Is(err, personal.ErrAttachmentUnavailable) || len(output.Attachments) == 0 {
			return err
		}
		text, fallbackChunks, prepareErr := r.unavailableAttachmentReply(ctx, event)
		if prepareErr != nil {
			return prepareErr
		}
		fallback := personal.TurnOutput{TaskID: output.TaskID, TaskIDs: output.TaskIDs, ScheduleID: output.ScheduleID, Text: text, Purpose: output.Purpose}
		return r.Store.CompleteEvent(ctx, event.ID, fallback, fallbackChunks)
	}
	return nil
}

func (r *Runtime) context(ctx context.Context, event personal.Event) (personal.TurnInput, error) {
	input := personal.TurnInput{Event: event}
	var err error
	if event.Kind == personal.EventUser {
		input.PendingInputs, err = r.Store.PendingInputs(ctx, event.ID)
		if err != nil {
			return input, err
		}
	}
	if event.Kind == personal.EventTask {
		input.PendingTaskInputs, err = r.Store.PendingTaskInputs(ctx, event.TaskID)
		if err != nil {
			return input, err
		}
	}
	input.History, err = r.Store.History(ctx, event.ID, 20)
	if err != nil {
		return input, err
	}
	input.Tasks, err = r.Store.Tasks(ctx, 10)
	if err != nil {
		return input, err
	}
	if event.TaskID == 0 && event.ReplyToID != "" {
		referenced, err := r.Store.ReferencedTasks(ctx, event.ID)
		if err != nil {
			return input, err
		}
		for _, task := range referenced {
			input.ReferencedTaskIDs = append(input.ReferencedTaskIDs, task.ID)
			found := false
			for _, existing := range input.Tasks {
				if existing.ID == task.ID {
					found = true
					break
				}
			}
			if !found {
				input.Tasks = append(input.Tasks, task)
			}
		}
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
				input.Tasks = append([]personal.Task{*task}, input.Tasks...)
			}
		}
	}
	if event.TaskID > 0 {
		input.Schedule, err = r.Store.ScheduleForTask(ctx, event.TaskID)
		if err != nil {
			return input, err
		}
	}
	if input.Schedule != nil {
		input.Suggestions, err = r.Store.ScheduleSuggestions(ctx, input.Schedule.Schedule.ID)
		if err != nil {
			return input, err
		}
	} else if event.ScheduleID > 0 {
		input.Suggestions, err = r.Store.ScheduleSuggestions(ctx, event.ScheduleID)
		if err != nil {
			return input, err
		}
	}
	if event.ReplyOutboxID > 0 {
		input.RepliedMessage, err = r.Store.RepliedMessage(ctx, event.ReplyOutboxID)
		if err != nil {
			return input, err
		}
		replied, err := r.Store.ReplyScheduleSuggestions(ctx, event.ReplyOutboxID)
		if err != nil {
			return input, err
		}
		input.ReplySuggestions = replied
	}
	input.Memories, err = r.Store.Memories(ctx, "", 20)
	if err != nil {
		return input, err
	}
	return input, nil
}
