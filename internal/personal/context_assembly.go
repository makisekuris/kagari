package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
)

var materialIDPattern = regexp.MustCompile(`\bm_[A-Za-z0-9_]+`)

type assemblyTraceItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Bytes  int    `json:"bytes"`
	Reason string `json:"reason,omitempty"`
}

type runResultCard struct {
	Summary             string             `json:"summary,omitempty"`
	Sources             []domain.SourceRef `json:"sources,omitempty"`
	MaterialEvidenceIDs []string           `json:"material_evidence_ids,omitempty"`
	Files               []string           `json:"files,omitempty"`
	AttachmentIDs       []string           `json:"attachment_ids,omitempty"`
	ReadRun             string             `json:"read_run"`
}

// assembleTurn selects the optional conversation context around the current request.
// Required request fields remain intact; the complete request is measured with its
// actual instruction and registered tool schemas before the caller sends it.
func assembleTurn(ctx context.Context, input TurnInput, instruction string, tools []tool.BaseTool, limits inputbudget.Limits) (TurnInput, error) {
	limits, err := inputbudget.NewLimits(limits.Window, limits.Output)
	if err != nil {
		return TurnInput{}, err
	}
	toolInfos := make([]*schema.ToolInfo, 0, len(tools))
	for _, registered := range tools {
		if registered == nil {
			return TurnInput{}, errors.New("personal: nil tool in context assembly")
		}
		info, err := registered.Info(ctx)
		if err != nil {
			return TurnInput{}, fmt.Errorf("personal: read tool schema for context assembly: %w", err)
		}
		if info == nil {
			return TurnInput{}, errors.New("personal: tool returned nil schema during context assembly")
		}
		toolInfos = append(toolInfos, info)
	}

	assembled := copyTurnInput(input)
	assembled.Memories = nil // Memory is selected and injected as a separately tagged message.
	assembled.History = []Message{}
	assembled.Tasks = currentTurnTasks(input)
	for i, event := range input.PendingInputs {
		assembled.PendingInputs[i] = pendingEventReference(event)
	}
	for i, event := range input.PendingTaskInputs {
		assembled.PendingTaskInputs[i] = pendingEventReference(event)
	}
	protectedHistory := make(map[int]bool)
	for i, message := range input.History {
		if isExplicitReply(input, message) {
			protectedHistory[i] = true
			assembled.History = append(assembled.History, message)
		}
	}

	var runParts *runResultCard
	if input.CurrentRun != nil {
		run := *input.CurrentRun
		run.ContextManifest = ""
		run.Result = ""
		assembled.CurrentRun = &run
		if input.CurrentRun.Result != "" {
			parts := summarizeRunResult(*input.CurrentRun)
			runParts = &parts
			minimal := runResultCard{ReadRun: parts.ReadRun}
			run.Result, err = encodeRunResultCard(minimal)
			if err != nil {
				return TurnInput{}, err
			}
			assembled.CurrentRun = &run
		}
	}

	selected := []assemblyTraceItem{
		{Kind: "instruction", ID: "personal", Bytes: len(instruction)},
		{Kind: "event", ID: strconv.FormatInt(input.Event.ID, 10), Bytes: len(input.Event.Text)},
	}
	var omitted []assemblyTraceItem
	if input.RepliedMessage != nil {
		selected = append(selected, assemblyTraceItem{Kind: "replied_message", ID: replyMessageID(*input.RepliedMessage), Bytes: len(input.RepliedMessage.Text)})
	}
	if task, ok := currentTask(input); ok {
		raw, _ := json.Marshal(task)
		selected = append(selected, assemblyTraceItem{Kind: "current_task", ID: strconv.FormatInt(task.ID, 10), Bytes: len(raw)})
	}
	if input.CurrentRun != nil {
		selected = append(selected, assemblyTraceItem{Kind: "current_run_input", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(input.CurrentRun.Input)})
		selected = append(selected, assemblyTraceItem{Kind: "current_run_revision", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(strconv.FormatInt(input.CurrentRun.Revision, 10))})
		if runParts != nil {
			selected = append(selected, assemblyTraceItem{Kind: "current_run_result_entry", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(assembled.CurrentRun.Result)})
		}
	}
	for i, message := range input.History {
		if protectedHistory[i] {
			selected = append(selected, assemblyTraceItem{Kind: "replied_history", ID: historyMessageID(message), Bytes: historyMessageBytes(message)})
		}
	}
	for _, event := range input.PendingInputs {
		if event.Text != "" {
			selected = append(selected, assemblyTraceItem{Kind: "pending_input_reference", ID: strconv.FormatInt(event.ID, 10), Bytes: eventBytes(pendingEventReference(event))})
		}
	}
	for _, event := range input.PendingTaskInputs {
		if event.Text != "" {
			selected = append(selected, assemblyTraceItem{Kind: "pending_task_input_reference", ID: strconv.FormatInt(event.ID, 10), Bytes: eventBytes(pendingEventReference(event))})
		}
	}

	baseEstimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
	if err != nil {
		return TurnInput{}, err
	}
	if baseEstimate.TotalBytes > limits.Hard {
		logAssembly(ctx, withToolEstimate(selected, baseEstimate.ToolsBytes), omitted, baseEstimate, limits)
		return TurnInput{}, assemblyLimitError(baseEstimate, limits)
	}

	selectedTasks := taskIDSet(assembled.Tasks)
	selectedCards := map[int64]bool{}
	addTaskCard := func(task Task, reason string) error {
		if selectedCards[task.ID] || selectedTasks[task.ID] {
			return nil
		}
		card := assemblyTaskCard(task)
		assembled.Tasks = append(assembled.Tasks, card)
		estimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
		if err != nil {
			assembled.Tasks = assembled.Tasks[:len(assembled.Tasks)-1]
			return err
		}
		if estimate.TotalBytes > limits.Target {
			assembled.Tasks = assembled.Tasks[:len(assembled.Tasks)-1]
			raw, _ := json.Marshal(card)
			omitted = append(omitted, assemblyTraceItem{Kind: "task_card", ID: strconv.FormatInt(task.ID, 10), Bytes: len(raw), Reason: "target"})
			return nil
		}
		selectedCards[task.ID] = true
		raw, _ := json.Marshal(card)
		selected = append(selected, assemblyTraceItem{Kind: "task_card", ID: strconv.FormatInt(task.ID, 10), Bytes: len(raw), Reason: reason})
		return nil
	}

	tasksByID := make(map[int64]Task, len(input.Tasks))
	for _, task := range input.Tasks {
		if _, exists := tasksByID[task.ID]; !exists {
			tasksByID[task.ID] = task
		}
	}
	for _, id := range input.ReferencedTaskIDs {
		if task, ok := tasksByID[id]; ok {
			if err := addTaskCard(task, "replied_task"); err != nil {
				return TurnInput{}, err
			}
		}
	}
	for i, event := range input.PendingTaskInputs {
		if err := expandPendingEvent(ctx, &assembled, &assembled.PendingTaskInputs[i], event, instruction, toolInfos, limits.Target); err != nil {
			return TurnInput{}, err
		}
		if event.Text != "" {
			if assembled.PendingTaskInputs[i].Text == event.Text {
				selected = append(selected, assemblyTraceItem{Kind: "pending_task_input", ID: strconv.FormatInt(event.ID, 10), Bytes: len(event.Text)})
			} else {
				omitted = append(omitted, assemblyTraceItem{Kind: "pending_task_input", ID: strconv.FormatInt(event.ID, 10), Bytes: len(event.Text), Reason: "get_event"})
			}
		}
	}
	for i, event := range input.PendingInputs {
		if err := expandPendingEvent(ctx, &assembled, &assembled.PendingInputs[i], event, instruction, toolInfos, limits.Target); err != nil {
			return TurnInput{}, err
		}
		if event.Text != "" {
			if assembled.PendingInputs[i].Text == event.Text {
				selected = append(selected, assemblyTraceItem{Kind: "pending_input", ID: strconv.FormatInt(event.ID, 10), Bytes: len(event.Text)})
			} else {
				omitted = append(omitted, assemblyTraceItem{Kind: "pending_input", ID: strconv.FormatInt(event.ID, 10), Bytes: len(event.Text), Reason: "get_event"})
			}
		}
	}

	if runParts != nil {
		refs := *runParts
		refs.Summary = ""
		resultCard := runResultCard{ReadRun: runParts.ReadRun}
		if len(refs.Sources)+len(refs.MaterialEvidenceIDs)+len(refs.Files)+len(refs.AttachmentIDs) > 0 {
			run := *assembled.CurrentRun
			run.Result, err = encodeRunResultCard(refs)
			if err != nil {
				return TurnInput{}, err
			}
			assembled.CurrentRun = &run
			estimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
			if err != nil {
				return TurnInput{}, err
			}
			if estimate.TotalBytes <= limits.Target {
				resultCard = refs
				selected = append(selected, assemblyTraceItem{Kind: "current_run_result_references", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(run.Result)})
			} else {
				omitted = append(omitted, assemblyTraceItem{Kind: "current_run_result_references", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(run.Result), Reason: "target"})
				run.Result, err = encodeRunResultCard(runResultCard{ReadRun: runParts.ReadRun})
				if err != nil {
					return TurnInput{}, err
				}
				assembled.CurrentRun = &run
			}
		}
		if runParts.Summary != "" {
			base := *assembled.CurrentRun
			best, bestRunes, err := bestRunSummary(ctx, assembled, instruction, toolInfos, limits.Target, base, resultCard, runParts.Summary)
			if err != nil {
				return TurnInput{}, err
			}
			if bestRunes > 0 {
				assembled = best
				selected = append(selected, assemblyTraceItem{Kind: "current_run_summary", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(assembled.CurrentRun.Result)})
			} else {
				omitted = append(omitted, assemblyTraceItem{Kind: "current_run_summary", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(runParts.Summary), Reason: "target"})
			}
		}
	}

	estimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
	if err != nil {
		return TurnInput{}, err
	}
	if estimate.TotalBytes > limits.Hard {
		logAssembly(ctx, withToolEstimate(selected, estimate.ToolsBytes), omitted, estimate, limits)
		return TurnInput{}, assemblyLimitError(estimate, limits)
	}

	historyOrder := historyPriority(input, protectedHistory)
	selectedHistory := make(map[int]bool, len(protectedHistory))
	for i := range protectedHistory {
		selectedHistory[i] = true
	}
	for _, index := range historyOrder {
		selectedHistory[index] = true
		assembled.History = orderedHistory(input.History, selectedHistory)
		estimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
		if err != nil {
			return TurnInput{}, err
		}
		if estimate.TotalBytes > limits.Target {
			delete(selectedHistory, index)
			assembled.History = orderedHistory(input.History, selectedHistory)
			message := input.History[index]
			omitted = append(omitted, assemblyTraceItem{Kind: "history", ID: historyMessageID(message), Bytes: historyMessageBytes(message), Reason: "target"})
			continue
		}
		message := input.History[index]
		selected = append(selected, assemblyTraceItem{Kind: "history", ID: historyMessageID(message), Bytes: historyMessageBytes(message)})
	}

	for _, task := range input.Tasks {
		if task.ID == input.Event.TaskID || containsInt64(input.ReferencedTaskIDs, task.ID) {
			continue
		}
		if err := addTaskCard(task, "unrelated_task"); err != nil {
			return TurnInput{}, err
		}
	}

	finalEstimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
	if err != nil {
		return TurnInput{}, err
	}
	if finalEstimate.TotalBytes > limits.Hard {
		logAssembly(ctx, withToolEstimate(selected, finalEstimate.ToolsBytes), omitted, finalEstimate, limits)
		return TurnInput{}, assemblyLimitError(finalEstimate, limits)
	}
	selected = withToolEstimate(selected, finalEstimate.ToolsBytes)
	logAssembly(ctx, selected, omitted, finalEstimate, limits)
	return assembled, nil
}

func copyTurnInput(input TurnInput) TurnInput {
	out := input
	out.PendingInputs = append([]Event(nil), input.PendingInputs...)
	out.PendingTaskInputs = append([]Event(nil), input.PendingTaskInputs...)
	out.ReferencedTaskIDs = append([]int64(nil), input.ReferencedTaskIDs...)
	out.ReplySuggestions = append([]ScheduleSuggestion(nil), input.ReplySuggestions...)
	out.Suggestions = append([]ScheduleSuggestion(nil), input.Suggestions...)
	out.History = append([]Message(nil), input.History...)
	out.Tasks = append([]Task(nil), input.Tasks...)
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

func currentTurnTasks(input TurnInput) []Task {
	task, ok := currentTask(input)
	if !ok {
		return []Task{}
	}
	return []Task{task}
}

func currentTask(input TurnInput) (Task, bool) {
	if input.Event.TaskID == 0 {
		return Task{}, false
	}
	for _, task := range input.Tasks {
		if task.ID == input.Event.TaskID {
			return task, true
		}
	}
	return Task{}, false
}

func assemblyTaskCard(task Task) Task {
	return Task{
		ID: task.ID, Title: task.Title, Revision: task.Revision, Status: task.Status,
		SourceRunIDs: append([]int64(nil), task.SourceRunIDs...), UpdatedAt: task.UpdatedAt,
	}
}

func pendingEventReference(event Event) Event {
	if event.Text != "" {
		event.Text = fmt.Sprintf("Event body is available through get_event event_id=%d.", event.ID)
	}
	return event
}

func eventBytes(event Event) int {
	raw, _ := json.Marshal(event)
	return len(raw)
}

func expandPendingEvent(ctx context.Context, assembled *TurnInput, target *Event, full Event, instruction string, tools []*schema.ToolInfo, budget int64) error {
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

func taskIDSet(tasks []Task) map[int64]bool {
	out := make(map[int64]bool, len(tasks))
	for _, task := range tasks {
		out[task.ID] = true
	}
	return out
}

func summarizeRunResult(run Run) runResultCard {
	card := runResultCard{}
	if run.ID > 0 {
		card.ReadRun = fmt.Sprintf("read_run run_id=%d section=result", run.ID)
	}
	if strings.TrimSpace(run.Result) == "" {
		return card
	}
	var result StageResult
	if json.Unmarshal([]byte(run.Result), &result) == nil && strings.HasPrefix(strings.TrimSpace(run.Result), "{") {
		card.Summary = result.Summary
		card.Sources = append([]domain.SourceRef(nil), result.Sources...)
		for _, progress := range result.Progress {
			for _, evidence := range progress.Evidence {
				if evidence.Kind == "material" {
					card.MaterialEvidenceIDs = append(card.MaterialEvidenceIDs, evidence.ID)
				}
			}
		}
		card.Files = append([]string(nil), result.Files...)
		for _, attachment := range result.Attachments {
			card.AttachmentIDs = append(card.AttachmentIDs, attachment.ID)
		}
	} else {
		card.Summary = run.Result
	}
	for _, id := range materialIDPattern.FindAllString(card.Summary, -1) {
		card.MaterialEvidenceIDs = append(card.MaterialEvidenceIDs, id)
	}
	card.MaterialEvidenceIDs = uniqueStrings(card.MaterialEvidenceIDs)
	for _, id := range card.MaterialEvidenceIDs {
		card.Summary = strings.ReplaceAll(card.Summary, id, "")
	}
	card.AttachmentIDs = uniqueStrings(card.AttachmentIDs)
	return card
}

func encodeRunResultCard(card runResultCard) (string, error) {
	encoded, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("personal: encode bounded current run result: %w", err)
	}
	return string(encoded), nil
}

func bestRunSummary(ctx context.Context, input TurnInput, instruction string, tools []*schema.ToolInfo, target int64, base Run, card runResultCard, summary string) (TurnInput, int, error) {
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
			return TurnInput{}, 0, err
		}
		run.Result = encoded
		candidate := input
		candidateRun := run
		candidate.CurrentRun = &candidateRun
		estimate, err := estimateTurn(ctx, candidate, instruction, tools)
		if err != nil {
			return TurnInput{}, 0, err
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

func estimateTurn(ctx context.Context, input TurnInput, instruction string, tools []*schema.ToolInfo) (inputbudget.Estimate, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return inputbudget.Estimate{}, fmt.Errorf("personal: encode assembled turn: %w", err)
	}
	messages := []*schema.AgenticMessage{schema.SystemAgenticMessage(instruction), schema.UserAgenticMessage(string(encoded))}
	return inputbudget.EstimateAgenticInput(messages, tools, nil, nil, nil, len(messages))
}

func assemblyLimitError(estimate inputbudget.Estimate, limits inputbudget.Limits) error {
	kind := inputbudget.ErrorHistoryExceedsLimit
	schemaBytes := estimate.ToolsBytes + estimate.OutputSchemaBytes
	if schemaBytes >= limits.Hard {
		kind = inputbudget.ErrorToolsSchemaExceedsLimit
	} else if estimate.RequiredBytes > limits.Hard-schemaBytes {
		kind = inputbudget.ErrorRequiredInputExceedsLimit
	}
	return &inputbudget.ContextLimitError{Kind: kind, Estimate: estimate, Limit: limits.Hard}
}

func historyPriority(input TurnInput, protected map[int]bool) []int {
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

func orderedHistory(history []Message, selected map[int]bool) []Message {
	out := make([]Message, 0, len(selected))
	for i, message := range history {
		if selected[i] {
			out = append(out, message)
		}
	}
	return out
}

func isExplicitReply(input TurnInput, message Message) bool {
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

func replyMessageID(message Message) string {
	if message.HistoryID > 0 {
		return strconv.FormatInt(message.HistoryID, 10)
	}
	if message.OutboxID > 0 {
		return "outbox:" + strconv.FormatInt(message.OutboxID, 10)
	}
	return strconv.FormatInt(message.EventID, 10)
}

func historyMessageID(message Message) string {
	if message.HistoryID > 0 {
		return strconv.FormatInt(message.HistoryID, 10)
	}
	return strconv.FormatInt(message.EventID, 10)
}

func historyMessageBytes(message Message) int {
	raw, _ := json.Marshal(message)
	return len(raw)
}

func containsInt64(values []int64, value int64) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
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
