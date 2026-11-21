package assistant

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/components/tool"
	"go.uber.org/zap"

	"kagari/internal/personal"
)

type personalToolReply struct {
	ActionID   string                          `json:"action_id,omitempty"`
	OK         bool                            `json:"ok"`
	Error      string                          `json:"error,omitempty"`
	Queued     bool                            `json:"queued,omitempty"`
	Task       *personal.Task                  `json:"task,omitempty"`
	Tasks      []personal.Task                 `json:"tasks,omitempty"`
	Run        *personal.Run                   `json:"run,omitempty"`
	Runs       []personal.Run                  `json:"runs,omitempty"`
	Memory     *personal.Memory                `json:"memory,omitempty"`
	Memories   []personal.Memory               `json:"memories,omitempty"`
	Delivery   *personal.DeliveryIntent        `json:"delivery,omitempty"`
	Deliveries *personal.DeliveryPage          `json:"deliveries,omitempty"`
	Control    *personal.DeliveryControlResult `json:"control,omitempty"`
}

type memoryCurrentAnswerReply struct {
	OK           bool              `json:"ok"`
	CurrentMatch bool              `json:"current_match"`
	Tombstone    bool              `json:"tombstone,omitempty"`
	Memory       *personal.Memory  `json:"memory,omitempty"`
	Memories     []personal.Memory `json:"memories,omitempty"`
	Error        string            `json:"error,omitempty"`
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
	Summary          string                    `json:"summary" jsonschema:"description=Required internal task summary saved on the task. This is not the user-facing message. Keep the actual result, failure, or remaining need and supporting source URLs; do not copy IDs or tool logs."`
	Delivery         personal.DeliveryDraft    `json:"delivery" jsonschema:"description=The user-facing closing delivery intent saved atomically with the task status. Set text to the plain-language message and attachments to the explicit ordered file IDs selected for this delivery. Use an empty attachments array when no file should be sent. Text may be empty only for an attachment-only delivery."`
}

type runArgs struct {
	RunID int64 `json:"run_id"`
}

type memorySearchArgs struct {
	Query string `json:"query"`
}

type memoryCurrentAnswerArgs struct {
	Query string `json:"query,omitempty" jsonschema:"description=Relevant words from the current direct question. Supply this unless an exact key alone is sufficient."`
	Key   string `json:"key,omitempty" jsonschema:"description=Exact memory key when it is already known. Never invent a key."`
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
	timezone              string
	scheduleIDs           map[int64]struct{}
	scheduleChanged       bool
	store                 Repository
	log                   *zap.Logger
	input                 personal.TurnInput
	event                 personal.Event
	taskIDs               map[int64]struct{}
	mustNotify            bool
	questions             []string
	settledDelivery       *personal.DeliveryDraft
	callbackFailure       *taskActionFailure
	controlDeliveries     []personal.DeliveryIntent
	persistedControl      bool
	currentMemoryMatched  bool
	currentMemoryTried    bool
	otherToolCalled       bool
	supplementalWork      bool
	supplementalTaskIDs   map[int64]struct{}
	supplementalSchedules map[int64]struct{}
	supplementalPurpose   personal.DeliveryPurpose
	deliveryQueried       bool
	taskResultCommitted   bool
	taskActions           map[string]*taskActionState
	taskActionOrder       []string
	nextTaskActionID      uint64
}

func (p *personalTools) recordCurrentMemoryAnswer(matched bool) {
	p.currentMemoryTried = true
	p.currentMemoryMatched = p.currentMemoryMatched || matched
}

func (p *personalTools) currentMemoryAnswerReply(event personal.Event, required string) (string, bool) {
	if !p.currentMemoryTried || p.currentMemoryMatched || p.otherToolCalled || event.Kind != personal.EventUser || required != "" {
		return "", false
	}
	return "当前没有查到匹配的有效记忆。", true
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
		if p.taskIDs == nil {
			p.taskIDs = make(map[int64]struct{})
		}
		p.taskIDs[id] = struct{}{}
	}
}

func (p *personalTools) taskIDsSorted() []int64 {
	ids := make([]int64, 0, len(p.taskIDs)+1)
	seen := make(map[int64]struct{}, len(p.taskIDs)+1)
	add := func(id int64) {
		if id <= 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range p.taskIDs {
		add(id)
	}
	for _, failure := range p.unresolvedTaskActions() {
		add(failure.TaskID)
	}
	if len(ids) == 0 {
		if id := p.taskID(); id > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (p *personalTools) addControlDelivery(intent personal.DeliveryIntent) {
	for _, existing := range p.controlDeliveries {
		if existing.ID == intent.ID {
			return
		}
	}
	p.controlDeliveries = append(p.controlDeliveries, intent)
	for _, scope := range intent.Scopes {
		p.addTaskID(scope.TaskID)
	}
	p.persistedControl = true
	p.mustNotify = true
}

func (p *personalTools) recordSupplementalWork(purpose personal.DeliveryPurpose) {
	p.supplementalWork = true
	if supplementalPurposeRank(purpose) > supplementalPurposeRank(p.supplementalPurpose) {
		p.supplementalPurpose = purpose
	}
}

func (p *personalTools) addSupplementalTask(id int64, purpose personal.DeliveryPurpose) {
	if id > 0 {
		if p.supplementalTaskIDs == nil {
			p.supplementalTaskIDs = make(map[int64]struct{})
		}
		p.supplementalTaskIDs[id] = struct{}{}
	}
	p.recordSupplementalWork(purpose)
}

func (p *personalTools) addSupplementalSchedule(id int64, purpose personal.DeliveryPurpose) {
	if id > 0 {
		if p.supplementalSchedules == nil {
			p.supplementalSchedules = make(map[int64]struct{})
		}
		p.supplementalSchedules[id] = struct{}{}
	}
	p.recordSupplementalWork(purpose)
}

func (p *personalTools) supplementalTaskIDsSorted() []int64 {
	ids := make([]int64, 0, len(p.supplementalTaskIDs))
	for id := range p.supplementalTaskIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (p *personalTools) supplementalScheduleID() int64 {
	if len(p.supplementalSchedules) != 1 {
		return 0
	}
	for id := range p.supplementalSchedules {
		return id
	}
	return 0
}

func supplementalPurposeRank(purpose personal.DeliveryPurpose) int {
	switch purpose {
	case personal.DeliveryResult:
		return 3
	case personal.DeliveryStatus:
		return 2
	case personal.DeliveryControl:
		return 1
	default:
		return 0
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

// build registers the task, memory, delivery, and query tools. Schedule, reply,
// attachment, context, and progress tools are appended by their focused files.
func (p *personalTools) build() ([]tool.BaseTool, error) {
	taskTools, err := p.taskTools()
	if err != nil {
		return nil, err
	}
	deliveryTools, err := p.deliveryTools()
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
	if p.event.Kind == personal.EventUser {
		reminders, err := p.reminderTools()
		if err != nil {
			return nil, err
		}
		scheduled = append(scheduled, reminders...)
	}
	inputs, err := p.inputTools()
	if err != nil {
		return nil, err
	}
	scheduled = append(scheduled, inputs...)
	tools := append(append(taskTools, memoryTools...), deliveryTools...)
	return append(tools, scheduled...), nil
}
