package assistant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"go.uber.org/zap"

	"kagari/internal/personal"
)

type personalToolReply struct {
	ActionID string            `json:"action_id,omitempty"`
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Queued   bool              `json:"queued,omitempty"`
	Task     *personal.Task    `json:"task,omitempty"`
	Tasks    []personal.Task   `json:"tasks,omitempty"`
	Run      *personal.Run     `json:"run,omitempty"`
	Runs     []personal.Run    `json:"runs,omitempty"`
	Memory   *personal.Memory  `json:"memory,omitempty"`
	Memories []personal.Memory `json:"memories,omitempty"`
}

type createTaskArgs struct {
	ActionID     string   `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	SourceRunIDs []int64  `json:"source_run_ids,omitempty"`
	MemoryRefs   []string `json:"memory_refs,omitempty"`
	Title        string   `json:"title"`
	Goal         string   `json:"goal"`
	Context      string   `json:"context"`
	Attachments  []string `json:"attachments,omitempty"`
}

type taskArgs struct {
	TaskID int64 `json:"task_id"`
}

type reviseTaskArgs struct {
	ActionID         string   `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	SourceRunIDs     []int64  `json:"source_run_ids,omitempty"`
	MemoryRefs       []string `json:"memory_refs,omitempty"`
	TaskID           int64    `json:"task_id"`
	ExpectedRevision int64    `json:"expected_revision"`
	Goal             string   `json:"goal"`
	Context          string   `json:"context"`
	Attachments      []string `json:"attachments,omitempty"`
}

type cancelTaskArgs struct {
	TaskID           int64 `json:"task_id"`
	ExpectedRevision int64 `json:"expected_revision"`
}

type continueTaskArgs struct {
	ActionID         string `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	TaskID           int64  `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Step             string `json:"step"`
	ExtendBudget     bool   `json:"extend_budget"`
}

type settleTaskArgs struct {
	Assessment       *personal.CheckAssessment `json:"check_assessment,omitempty"`
	TaskID           int64                     `json:"task_id"`
	ExpectedRevision int64                     `json:"expected_revision"`
	Status           personal.TaskStatus       `json:"status"`
	Summary          string                    `json:"summary" jsonschema:"description=The task closing message sent directly to the user. Write plain text with direct source URLs; do not use Markdown headings or bold or link syntax. Do not claim internal stage findings were already reported unless the chat history contains that report."`
}

type runArgs struct {
	RunID int64 `json:"run_id"`
}

type memorySearchArgs struct {
	Query string `json:"query"`
}

type memoryKeyArgs struct {
	Key string `json:"key"`
}

type memoryPutArgs struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	ExpectedVersion int64  `json:"expected_version"`
	Evidence        string `json:"evidence"`
}

type memoryForgetArgs struct {
	Key             string `json:"key"`
	ExpectedVersion int64  `json:"expected_version"`
	Evidence        string `json:"evidence"`
}

type memoryHistoryArgs struct {
	Key string `json:"key,omitempty"`
}

// personalTools carries request scope and the in-memory action ledger shared by
// the task, memory, schedule, reply, and attachment tool groups.
type personalTools struct {
	timezone         string
	scheduleIDs      map[int64]struct{}
	scheduleChanged  bool
	store            Repository
	log              *zap.Logger
	input            personal.TurnInput
	event            personal.Event
	taskIDs          map[int64]struct{}
	mustNotify       bool
	questions        []string
	taskActions      map[string]*taskActionState
	taskActionOrder  []string
	nextTaskActionID uint64
}

func (p *personalTools) addScheduleID(id int64, changed bool) {
	if id <= 0 {
		return
	}
	if changed && !p.scheduleChanged {
		p.scheduleIDs = nil
		p.scheduleChanged = true
	} else if !changed && p.scheduleChanged {
		return
	}
	if p.scheduleIDs == nil {
		p.scheduleIDs = make(map[int64]struct{})
	}
	p.scheduleIDs[id] = struct{}{}
}

func (p *personalTools) scheduleID() int64 {
	if len(p.scheduleIDs) == 0 {
		if p.taskID() != p.event.TaskID {
			return 0
		}
		return p.event.ScheduleID
	}
	if len(p.scheduleIDs) == 1 {
		for id := range p.scheduleIDs {
			return id
		}
	}
	return 0
}

func (p *personalTools) taskID() int64 {
	if len(p.taskIDs) == 0 && len(p.scheduleIDs) > 0 {
		if _, sameSchedule := p.scheduleIDs[p.event.ScheduleID]; len(p.scheduleIDs) != 1 || !sameSchedule {
			return 0
		}
	}
	if p.event.TaskID > 0 {
		if len(p.taskIDs) == 0 {
			return p.event.TaskID
		}
		if _, ok := p.taskIDs[p.event.TaskID]; !ok {
			return 0
		}
	}
	if len(p.taskIDs) == 0 {
		return p.event.TaskID
	}
	if len(p.taskIDs) == 1 {
		for id := range p.taskIDs {
			return id
		}
	}
	return 0
}

func (p *personalTools) addTaskID(id int64) {
	if id > 0 {
		p.taskIDs[id] = struct{}{}
	}
}

func (p *personalTools) directUserMutation() error {
	if p.event.Kind != personal.EventUser || p.event.ID <= 0 {
		return fmt.Errorf("this operation requires a persisted direct user event")
	}
	return nil
}

func (p *personalTools) ownTask(taskID int64) error {
	if p.event.Kind != personal.EventTask || p.event.ID <= 0 || p.event.TaskID <= 0 || taskID != p.event.TaskID {
		return fmt.Errorf("task callbacks may only change their own task")
	}
	return nil
}

func (p *personalTools) requestKey(name string, input any) (string, error) {
	canonical, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode tool input: %w", err)
	}
	hash := sha256.Sum256(canonical)
	return fmt.Sprintf("event:%d:%s:%s", p.event.ID, name, hex.EncodeToString(hash[:])), nil
}

func toolError(err error) personalToolReply {
	return personalToolReply{OK: false, Error: err.Error()}
}

func (p *personalTools) memoryError(key, operation string, err error) personalToolReply {
	if p.log != nil {
		p.log.Warn("personal memory change failed", zap.Int64("event_id", p.event.ID), zap.String("key", key), zap.String("operation", operation), zap.Error(err))
	}
	return toolError(err)
}

func (p *personalTools) taskByID(id int64) *personal.Task {
	for i := range p.input.Tasks {
		if p.input.Tasks[i].ID == id {
			return &p.input.Tasks[i]
		}
	}
	return nil
}

// build registers the core task, memory, and query tools. Schedule, reply,
// attachment, context, and progress tools are appended by their focused files.
func (p *personalTools) build() ([]tool.BaseTool, error) {
	taskTools, err := p.taskTools()
	if err != nil {
		return nil, err
	}
	memoryTools, err := p.memoryTools()
	if err != nil {
		return nil, err
	}
	scheduled, err := p.buildScheduleTools()
	if err != nil {
		return nil, err
	}
	suggestions, err := p.buildScheduleSuggestionTools()
	if err != nil {
		return nil, err
	}
	scheduled = append(scheduled, suggestions...)
	inputs, err := p.inputTools()
	if err != nil {
		return nil, err
	}
	scheduled = append(scheduled, inputs...)
	return append(append(taskTools, memoryTools...), scheduled...), nil
}
