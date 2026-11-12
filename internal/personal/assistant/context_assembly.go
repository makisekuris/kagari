package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/inputbudget"
	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
)

type assemblyTraceItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Bytes  int    `json:"bytes"`
	Reason string `json:"reason,omitempty"`
}

// Turn context selection keeps the current request and active task requirements protected.
// assembleTurn selects the optional conversation context around the current request.
// Required request fields remain intact; the complete request is measured with its
// actual instruction and registered tool schemas before the caller sends it.
func assembleTurn(ctx context.Context, input personal.TurnInput, instruction string, tools []tool.BaseTool, limits inputbudget.Limits) (personal.TurnInput, error) {
	limits, err := inputbudget.NewLimits(limits.Window, limits.Output)
	if err != nil {
		return personal.TurnInput{}, err
	}
	toolInfos := make([]*schema.ToolInfo, 0, len(tools))
	for _, registered := range tools {
		if registered == nil {
			return personal.TurnInput{}, errors.New("personal: nil tool in context assembly")
		}
		info, err := registered.Info(ctx)
		if err != nil {
			return personal.TurnInput{}, fmt.Errorf("personal: read tool schema for context assembly: %w", err)
		}
		if info == nil {
			return personal.TurnInput{}, errors.New("personal: tool returned nil schema during context assembly")
		}
		toolInfos = append(toolInfos, info)
	}

	assembled := copyTurnInput(input)
	assembled.Memories = nil // Memory is selected and injected as a separately tagged message.
	assembled.History = []personal.Message{}
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

	var runParts *personal.RunResultCard
	if input.CurrentRun != nil {
		run := *input.CurrentRun
		run.ContextManifest = ""
		run.Result = ""
		assembled.CurrentRun = &run
		if input.CurrentRun.Result != "" {
			parts := personal.SummarizeRunResult(*input.CurrentRun)
			runParts = &parts
			minimal := personal.RunResultCard{ReadRun: parts.ReadRun}
			run.Result, err = encodeRunResultCard(minimal)
			if err != nil {
				return personal.TurnInput{}, err
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
		return personal.TurnInput{}, err
	}
	if baseEstimate.TotalBytes > limits.Hard {
		logAssembly(ctx, withToolEstimate(selected, baseEstimate.ToolsBytes), omitted, baseEstimate, limits)
		return personal.TurnInput{}, agentctx.AssemblyLimitError(baseEstimate, limits)
	}

	selectedTasks := taskIDSet(assembled.Tasks)
	selectedCards := map[int64]bool{}
	addTaskCard := func(task personal.Task, reason string) error {
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

	tasksByID := make(map[int64]personal.Task, len(input.Tasks))
	for _, task := range input.Tasks {
		if _, exists := tasksByID[task.ID]; !exists {
			tasksByID[task.ID] = task
		}
	}
	for _, id := range input.ReferencedTaskIDs {
		if task, ok := tasksByID[id]; ok {
			if err := addTaskCard(task, "replied_task"); err != nil {
				return personal.TurnInput{}, err
			}
		}
	}
	for i, event := range input.PendingTaskInputs {
		if err := expandPendingEvent(ctx, &assembled, &assembled.PendingTaskInputs[i], event, instruction, toolInfos, limits.Target); err != nil {
			return personal.TurnInput{}, err
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
			return personal.TurnInput{}, err
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
		resultCard := personal.RunResultCard{ReadRun: runParts.ReadRun}
		if len(refs.Sources)+len(refs.MaterialEvidenceIDs)+len(refs.Files)+len(refs.AttachmentIDs) > 0 {
			run := *assembled.CurrentRun
			run.Result, err = encodeRunResultCard(refs)
			if err != nil {
				return personal.TurnInput{}, err
			}
			assembled.CurrentRun = &run
			estimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
			if err != nil {
				return personal.TurnInput{}, err
			}
			if estimate.TotalBytes <= limits.Target {
				resultCard = refs
				selected = append(selected, assemblyTraceItem{Kind: "current_run_result_references", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(run.Result)})
			} else {
				omitted = append(omitted, assemblyTraceItem{Kind: "current_run_result_references", ID: strconv.FormatInt(input.CurrentRun.ID, 10), Bytes: len(run.Result), Reason: "target"})
				run.Result, err = encodeRunResultCard(personal.RunResultCard{ReadRun: runParts.ReadRun})
				if err != nil {
					return personal.TurnInput{}, err
				}
				assembled.CurrentRun = &run
			}
		}
		if runParts.Summary != "" {
			base := *assembled.CurrentRun
			best, bestRunes, err := bestRunSummary(ctx, assembled, instruction, toolInfos, limits.Target, base, resultCard, runParts.Summary)
			if err != nil {
				return personal.TurnInput{}, err
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
		return personal.TurnInput{}, err
	}
	if estimate.TotalBytes > limits.Hard {
		logAssembly(ctx, withToolEstimate(selected, estimate.ToolsBytes), omitted, estimate, limits)
		return personal.TurnInput{}, agentctx.AssemblyLimitError(estimate, limits)
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
			return personal.TurnInput{}, err
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
			return personal.TurnInput{}, err
		}
	}

	finalEstimate, err := estimateTurn(ctx, assembled, instruction, toolInfos)
	if err != nil {
		return personal.TurnInput{}, err
	}
	if finalEstimate.TotalBytes > limits.Hard {
		logAssembly(ctx, withToolEstimate(selected, finalEstimate.ToolsBytes), omitted, finalEstimate, limits)
		return personal.TurnInput{}, agentctx.AssemblyLimitError(finalEstimate, limits)
	}
	selected = withToolEstimate(selected, finalEstimate.ToolsBytes)
	logAssembly(ctx, selected, omitted, finalEstimate, limits)
	return assembled, nil
}
