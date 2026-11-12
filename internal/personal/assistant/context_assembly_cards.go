package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/inputbudget"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

// Context cards keep optional history, pending events, and run results bounded.
func copyTurnInput(input personal.TurnInput) personal.TurnInput {
	out := input
	out.PendingInputs = append([]personal.Event(nil), input.PendingInputs...)
	out.PendingTaskInputs = append([]personal.Event(nil), input.PendingTaskInputs...)
	out.ReferencedTaskIDs = append([]int64(nil), input.ReferencedTaskIDs...)
	out.ReplySuggestions = append([]personal.ScheduleSuggestion(nil), input.ReplySuggestions...)
	out.Suggestions = append([]personal.ScheduleSuggestion(nil), input.Suggestions...)
	out.History = append([]personal.Message(nil), input.History...)
	out.Tasks = append([]personal.Task(nil), input.Tasks...)
	if input.RepliedMessage != nil {
		replied := *input.RepliedMessage
		out.RepliedMessage = &replied
	}
	if input.Schedule != nil {
		schedule := *input.Schedule
		out.Schedule = &schedule
	}
	if input.CurrentRun != nil {
		run := *input.CurrentRun
		out.CurrentRun = &run
	}
	return out
}

func currentTurnTasks(input personal.TurnInput) []personal.Task {
	task, ok := currentTask(input)
	if !ok {
		return []personal.Task{}
	}
	return []personal.Task{task}
}

func currentTask(input personal.TurnInput) (personal.Task, bool) {
	if input.Event.TaskID == 0 {
		return personal.Task{}, false
	}
	for _, task := range input.Tasks {
		if task.ID == input.Event.TaskID {
			return task, true
		}
	}
	return personal.Task{}, false
}

func assemblyTaskCard(task personal.Task) personal.Task {
	return personal.Task{
		ID: task.ID, Title: task.Title, Revision: task.Revision, Status: task.Status,
		SourceRunIDs: append([]int64(nil), task.SourceRunIDs...), UpdatedAt: task.UpdatedAt,
	}
}

func pendingEventReference(event personal.Event) personal.Event {
	if event.Text != "" {
		event.Text = fmt.Sprintf("Event body is available through get_event event_id=%d.", event.ID)
	}
	return event
}

func eventBytes(event personal.Event) int {
	raw, _ := json.Marshal(event)
	return len(raw)
}

// Pending-event and run-result cards are compact references that can be expanded on demand.
func expandPendingEvent(ctx context.Context, assembled *personal.TurnInput, target *personal.Event, full personal.Event, instruction string, tools []*schema.ToolInfo, budget int64) error {
	if full.Text == "" {
		return nil
	}
	reference := *target
	*target = full
	estimate, err := estimateTurn(ctx, *assembled, instruction, tools)
	if err != nil {
		*target = reference
		return err
	}
	if estimate.TotalBytes > budget {
		*target = reference
	}
	return nil
}

func taskIDSet(tasks []personal.Task) map[int64]bool {
	out := make(map[int64]bool, len(tasks))
	for _, task := range tasks {
		out[task.ID] = true
	}
	return out
}

func encodeRunResultCard(card personal.RunResultCard) (string, error) {
	encoded, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("personal: encode bounded current run result: %w", err)
	}
	return string(encoded), nil
}

func bestRunSummary(ctx context.Context, input personal.TurnInput, instruction string, tools []*schema.ToolInfo, target int64, base personal.Run, card personal.RunResultCard, summary string) (personal.TurnInput, int, error) {
	runes := []rune(summary)
	low, high, bestSize := 0, len(runes), 0
	best := input
	for low <= high {
		middle := low + (high-low)/2
		candidateCard := card
		candidateCard.Summary = string(runes[:middle])
		if middle < len(runes) {
			candidateCard.Summary += "…"
		}
		run := base
		encoded, err := encodeRunResultCard(candidateCard)
		if err != nil {
			return personal.TurnInput{}, 0, err
		}
		run.Result = encoded
		candidate := input
		candidateRun := run
		candidate.CurrentRun = &candidateRun
		estimate, err := estimateTurn(ctx, candidate, instruction, tools)
		if err != nil {
			return personal.TurnInput{}, 0, err
		}
		if estimate.TotalBytes <= target {
			best, bestSize = candidate, middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best, bestSize, nil
}

func estimateTurn(ctx context.Context, input personal.TurnInput, instruction string, tools []*schema.ToolInfo) (inputbudget.Estimate, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return inputbudget.Estimate{}, fmt.Errorf("personal: encode assembled turn: %w", err)
	}
	messages := []*schema.AgenticMessage{schema.SystemAgenticMessage(instruction), schema.UserAgenticMessage(string(encoded))}
	return inputbudget.EstimateAgenticInput(messages, tools, nil, nil, nil, len(messages))
}

func historyPriority(input personal.TurnInput, protected map[int]bool) []int {
	var related, unrelated []int
	for i := len(input.History) - 1; i >= 0; i-- {
		if protected[i] {
			continue
		}
		message := input.History[i]
		if message.TaskID == input.Event.TaskID && input.Event.TaskID != 0 || containsInt64(input.ReferencedTaskIDs, message.TaskID) {
			related = append(related, i)
		} else {
			unrelated = append(unrelated, i)
		}
	}
	return append(related, unrelated...)
}

func orderedHistory(history []personal.Message, selected map[int]bool) []personal.Message {
	out := make([]personal.Message, 0, len(selected))
	for i, message := range history {
		if selected[i] {
			out = append(out, message)
		}
	}
	return out
}

func isExplicitReply(input personal.TurnInput, message personal.Message) bool {
	if input.RepliedMessage != nil {
		replied := input.RepliedMessage
		if replied.HistoryID > 0 && message.HistoryID == replied.HistoryID {
			return true
		}
		if replied.EventID > 0 && message.EventID == replied.EventID && message.Role == replied.Role {
			return true
		}
	}
	return input.Event.ReplyToID != "" && message.MessageID == input.Event.ReplyToID
}

func replyMessageID(message personal.Message) string {
	if message.HistoryID > 0 {
		return strconv.FormatInt(message.HistoryID, 10)
	}
	if message.OutboxID > 0 {
		return "outbox:" + strconv.FormatInt(message.OutboxID, 10)
	}
	return strconv.FormatInt(message.EventID, 10)
}

func historyMessageID(message personal.Message) string {
	if message.HistoryID > 0 {
		return strconv.FormatInt(message.HistoryID, 10)
	}
	return strconv.FormatInt(message.EventID, 10)
}

func historyMessageBytes(message personal.Message) int {
	raw, _ := json.Marshal(message)
	return len(raw)
}

// History ordering and assembly diagnostics.
func containsInt64(values []int64, value int64) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func logAssembly(ctx context.Context, selected, omitted []assemblyTraceItem, estimate inputbudget.Estimate, limits inputbudget.Limits) {
	logging.FromContext(ctx).Info("context.assembled",
		zap.Any("selected", selected), zap.Any("omitted", omitted),
		zap.Int64("estimated_input_bytes", estimate.TotalBytes), zap.Int64("hard_limit", limits.Hard),
		zap.Int64("target", limits.Target), zap.String("count_basis", inputbudget.CountBasis),
		zap.Int64("tool_schema_bytes", estimate.ToolsBytes),
	)
}

func withToolEstimate(selected []assemblyTraceItem, bytes int64) []assemblyTraceItem {
	items := append([]assemblyTraceItem(nil), selected...)
	items = append(items, assemblyTraceItem{Kind: "tool_schemas", Bytes: int(bytes)})
	return items
}
