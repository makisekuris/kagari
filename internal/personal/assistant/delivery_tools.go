package assistant

import (
	"context"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"go.uber.org/zap"

	"kagari/internal/personal"
)

type getDeliveryArgs struct {
	DeliveryID int64 `json:"delivery_id"`
}

type stopDeliveryArgs struct {
	ActionID   string `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. Reuse the same action_id when correcting or retrying this failed operation."`
	TaskID     int64  `json:"task_id,omitempty"`
	DeliveryID int64  `json:"delivery_id,omitempty"`
}

type resumeTaskDeliveryArgs struct {
	ActionID string `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. Reuse the same action_id when correcting or retrying this failed operation."`
	TaskID   int64  `json:"task_id"`
}

type resendDeliveryArgs struct {
	ActionID   string `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. Reuse the same action_id when correcting or retrying this failed operation."`
	DeliveryID int64  `json:"delivery_id"`
}

func deliveryPageLimit(limit int) (int, error) {
	if limit < 0 || limit > personal.MaxDeliveryPageSize {
		return 0, fmt.Errorf("limit must be between 0 and %d", personal.MaxDeliveryPageSize)
	}
	if limit == 0 {
		return personal.DefaultDeliveryPageSize, nil
	}
	return limit, nil
}

func (p *personalTools) deliveryTools() ([]tool.BaseTool, error) {
	list, err := utils.InferTool("list_deliveries", "查询交付用途、来源版本和逐项投递进度；返回省略正文的紧凑卡片，含每个 outbox 项的 ID、文字段/附件 ID、状态、失败原因、尝试次数、下次重试时间及已知回执。sent 回执只证明渠道接受了该项，不证明用户已阅读；部分成功只适用于具体已 sent 项。默认 20 条，最多 100 条，可按 task_id 筛选并用 before_id 向前翻页。仅直接用户事件可查询。", func(ctx context.Context, query personal.DeliveryQuery) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		if query.TaskID < 0 || query.BeforeID < 0 {
			return toolError(fmt.Errorf("task_id and before_id cannot be negative")), nil
		}
		limit, err := deliveryPageLimit(query.Limit)
		if err != nil {
			return toolError(err), nil
		}
		query.Limit = limit
		page, err := p.store.QueryDeliveries(ctx, personal.ReadScope{EventID: p.event.ID}, query)
		if err != nil {
			return toolError(err), nil
		}
		p.deliveryQueried = true
		p.recordSupplementalWork(personal.DeliveryControl)
		if query.TaskID > 0 {
			p.addTaskID(query.TaskID)
			p.addSupplementalTask(query.TaskID, personal.DeliveryControl)
		}
		p.addDeliveryPageTaskIDs(page)
		cards := make([]personal.DeliveryIntent, len(page.Items))
		for i, intent := range page.Items {
			cards[i] = personal.DeliveryCard(intent)
		}
		page.Items = cards
		return personalToolReply{OK: true, Deliveries: &page}, nil
	})
	if err != nil {
		return nil, err
	}

	get, err := utils.InferTool("get_delivery", "按交付 ID 查看生成的完整草案、所选附件、来源 Task 版本及逐项投递进度。每项 sent 回执表示渠道接受了该项，不表示用户已阅读；部分成功只证明具体已 sent 项。仅直接用户事件可查询。", func(ctx context.Context, args getDeliveryArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		if args.DeliveryID <= 0 {
			return toolError(fmt.Errorf("delivery_id must be positive")), nil
		}
		intent, err := p.loadDelivery(ctx, args.DeliveryID)
		if err != nil {
			return toolError(err), nil
		}
		p.deliveryQueried = true
		p.recordSupplementalWork(personal.DeliveryControl)
		p.addSupplementalDeliveryTaskIDs(*intent)
		return personalToolReply{OK: true, Delivery: intent}, nil
	})
	if err != nil {
		return nil, err
	}

	stop, err := utils.InferTool("stop_delivery", "明确阻止投递时使用；取消计算本身不会停止已有成果投递。必须且只能提供 task_id 或 delivery_id：任务范围停止所有版本的待发及后续成果，单份交付只停止该交付中尚未开始发送的内容。已发送或结果未知的内容不能保证撤回；仅直接用户事件可操作。操作失败后修正参数并复用 action_id。", func(ctx context.Context, args stopDeliveryArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		request := personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: args.TaskID, DeliveryID: args.DeliveryID}
		target := deliveryControlTarget(p, request)
		attempt, err := p.beginTaskAction("stop_delivery", args.ActionID, request.TaskID, target, request)
		if err != nil {
			return taskActionToolError(attempt.actionID, err), nil
		}
		fail := func(err error) (personalToolReply, error) { return p.failTaskAction(attempt, err), nil }
		if _, err := stopDeliveryRequest(args); err != nil {
			return fail(err)
		}
		if request.DeliveryID > 0 {
			if _, err := p.loadDelivery(ctx, request.DeliveryID); err != nil {
				return fail(err)
			}
		}
		return p.controlDelivery(ctx, "stop_delivery", attempt, request)
	})
	if err != nil {
		return nil, err
	}

	resume, err := utils.InferTool("resume_task_delivery", "明确恢复某个任务的未来成果投递时使用。只允许 task_id；仅允许后续新交付，不会复活已经停止或抑制的旧交付。仅直接用户事件可操作。操作失败后修正参数并复用 action_id。", func(ctx context.Context, args resumeTaskDeliveryArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		request := personal.DeliveryControlRequest{Action: personal.DeliveryResume, TaskID: args.TaskID}
		attempt, err := p.beginTaskAction("resume_task_delivery", args.ActionID, request.TaskID, deliveryControlTarget(p, request), request)
		if err != nil {
			return taskActionToolError(attempt.actionID, err), nil
		}
		if args.TaskID <= 0 {
			return p.failTaskAction(attempt, fmt.Errorf("task_id must be positive")), nil
		}
		return p.controlDelivery(ctx, "resume_task_delivery", attempt, request)
	})
	if err != nil {
		return nil, err
	}

	resend, err := utils.InferTool("resend_delivery", "仅在用户明确要求补发某份历史交付时使用。会创建引用原来源版本的新历史交付，不修改旧交付或回执；发送结果未知时不得自动补发，应说明结果未知，只有用户知情后仍明确要求补发时才创建新的历史交付。仅直接用户事件可操作。操作失败后修正参数并复用 action_id。", func(ctx context.Context, args resendDeliveryArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		request := personal.DeliveryControlRequest{Action: personal.DeliveryResend, DeliveryID: args.DeliveryID}
		attempt, err := p.beginTaskAction("resend_delivery", args.ActionID, 0, deliveryControlTarget(p, request), request)
		if err != nil {
			return taskActionToolError(attempt.actionID, err), nil
		}
		if args.DeliveryID <= 0 {
			return p.failTaskAction(attempt, fmt.Errorf("delivery_id must be positive")), nil
		}
		if _, err := p.loadDelivery(ctx, args.DeliveryID); err != nil {
			return p.failTaskAction(attempt, err), nil
		}
		return p.controlDelivery(ctx, "resend_delivery", attempt, request)
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{list, get, stop, resume, resend}, nil
}

func stopDeliveryRequest(args stopDeliveryArgs) (personal.DeliveryControlRequest, error) {
	if args.TaskID < 0 || args.DeliveryID < 0 || (args.TaskID > 0) == (args.DeliveryID > 0) {
		return personal.DeliveryControlRequest{}, fmt.Errorf("provide exactly one positive task_id or delivery_id")
	}
	return personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: args.TaskID, DeliveryID: args.DeliveryID}, nil
}

func (p *personalTools) addDeliveryPageTaskIDs(page personal.DeliveryPage) {
	p.recordSupplementalWork(personal.DeliveryControl)
	for _, intent := range page.Items {
		p.addDeliveryTaskIDs(intent)
		p.addSupplementalDeliveryTaskIDs(intent)
	}
	for _, taskID := range page.StoppedTaskIDs {
		p.addTaskID(taskID)
		p.addSupplementalTask(taskID, personal.DeliveryControl)
	}
}

func (p *personalTools) addSupplementalDeliveryTaskIDs(intent personal.DeliveryIntent) {
	for _, scope := range intent.Scopes {
		p.addSupplementalTask(scope.TaskID, personal.DeliveryControl)
	}
}

func (p *personalTools) addDeliveryTaskIDs(intent personal.DeliveryIntent) {
	for _, scope := range intent.Scopes {
		p.addTaskID(scope.TaskID)
	}
}

func (p *personalTools) captureTaskCancellationDeliveries(ctx context.Context) error {
	intents, err := p.store.EventDeliveries(ctx, p.event.ID)
	if err != nil {
		return err
	}
	found := false
	for _, intent := range intents {
		if intent.Purpose != personal.DeliveryControl {
			continue
		}
		p.addControlDelivery(intent)
		found = true
	}
	if !found {
		return fmt.Errorf("task cancellation has no saved control delivery")
	}
	return nil
}

func (p *personalTools) cancellationDeliveryPage() personal.DeliveryPage {
	items := append([]personal.DeliveryIntent(nil), p.controlDeliveries...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	return personal.DeliveryPage{Items: items, StoppedTaskIDs: []int64{}}
}

func (p *personalTools) loadDelivery(ctx context.Context, deliveryID int64) (*personal.DeliveryIntent, error) {
	intent, err := p.store.Delivery(ctx, personal.ReadScope{EventID: p.event.ID}, deliveryID)
	if err != nil {
		return nil, err
	}
	if intent == nil {
		return nil, fmt.Errorf("delivery not found")
	}
	p.addDeliveryTaskIDs(*intent)
	return intent, nil
}

func deliveryControlTarget(p *personalTools, request personal.DeliveryControlRequest) string {
	if request.TaskID > 0 {
		if title := p.taskActionName(request.TaskID); title != "" {
			return title
		}
		return "该任务"
	}
	return "这份交付"
}

func (p *personalTools) controlDelivery(ctx context.Context, operation string, attempt taskActionAttempt, request personal.DeliveryControlRequest) (personalToolReply, error) {
	fail := func(err error) (personalToolReply, error) { return p.failTaskAction(attempt, err), nil }
	key, err := p.requestKey(operation, request)
	if err != nil {
		return fail(err)
	}
	result, err := p.store.ControlDelivery(ctx, p.event.ID, key, request)
	if err != nil {
		return fail(err)
	}
	p.succeedTaskAction(attempt)
	p.persistedControl = true
	p.mustNotify = true
	p.addTaskID(request.TaskID)
	if result.ID <= 0 || result.DeliveryID <= 0 || result.Request != request {
		return personalToolReply{}, fmt.Errorf("personal: delivery control returned an invalid result")
	}
	intent, err := p.store.Delivery(ctx, personal.ReadScope{EventID: p.event.ID}, result.DeliveryID)
	if err != nil {
		if p.log != nil {
			p.log.Warn("delivery control intent reload failed", zap.Int64("event_id", p.event.ID), zap.Int64("delivery_id", result.DeliveryID), zap.Error(err))
		}
		requestCopy := result
		return personalToolReply{OK: true, ActionID: attempt.actionID, Control: &requestCopy}, nil
	}
	if intent == nil {
		if p.log != nil {
			p.log.Warn("delivery control intent reload returned no row", zap.Int64("event_id", p.event.ID), zap.Int64("delivery_id", result.DeliveryID))
		}
		requestCopy := result
		return personalToolReply{OK: true, ActionID: attempt.actionID, Control: &requestCopy}, nil
	}
	p.addDeliveryTaskIDs(*intent)
	p.addControlDelivery(*intent)
	requestCopy, intentCopy := result, *intent
	return personalToolReply{OK: true, ActionID: attempt.actionID, Control: &requestCopy, Delivery: &intentCopy}, nil
}
