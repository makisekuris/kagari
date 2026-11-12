# 定时计划实现方案

状态：2026-11-06，四个迭代已实现，工程检查与真实模型小样本验收通过。产品基线见 [定时计划设计](SCHEDULE-DESIGN.md)。本文替代此前的两表方案、任务等待导致计划暂停及入队时固定参考时间的方案。

## 1. 现有调用链和改动范围

复用 internal/personal 的 Store、单 SQLite 连接、Runtime.loop、Task worker 和 outbox。引入 robfig/cron/v3 v3.0.1 负责 cron 解析与时间计算，不使用它的内存 Job 调度器。

~~~text
用户 -> Think/计划工具 -> Store 计划写事务
Runtime.schedule -> DispatchDueSchedule -> 来源事件 + Task + 首 Run + occurrence
Runtime.task -> ClaimRun -> acquireTask -> BeginScheduledRun -> Execute
FinishStage -> EventTask -> 主 Agent 审查 -> SettleTask
                                           -> 成功检查进度，同一事务
CompleteEvent -> history + outbox -> deliver
~~~

现有 ClaimRun 已把 Task/Run 标为 running，之后 worker 才 acquireTask。因此新增首次执行事务必须在 acquireTask 成功之后、Execute 之前，不能把 ClaimRun 时间当成实际检查终点。

FinishStage 保存阶段结果并产生回调，不等于整个 Task 完成。推进检查进度挂在 SettleTask 的 completed 分支。CompleteEvent 的通知入队和渠道 Send 是后续边界，不能用送达作为检查完成条件。

## 2. 持久化契约

定时计划原实现使用 schema 3；与上下文管理、回复和输入关联整合后统一使用 schema 4，默认路径为 `data/kagari-context.db`。旧库拒绝启动但不删除，使用新的 storage.path；不编写迁移脚本。定时计划的触发、检查区间、成功进度和背景建议规则保持不变。

字段与约束对应 `personal/sqlite/schedule_schema.go`、`personal/sqlite/schema.go` 和 `personal/schedule_suggestion.go`；验证结果以本文件末尾的记录为准。沿用现有 ID、UTC 时间文本和 JSON 存储方式。

### 2.1 personal_schedules

| 字段 | 类型与含义 |
| --- | --- |
| id | INTEGER PRIMARY KEY |
| revision | 正整数；所有用户有效修改、自动暂停递增，用于写操作 CAS |
| scope_revision | 正整数；内容变更或从主动暂停恢复时递增，隔离检查进度；改名、改时、预算和故障恢复不递增 |
| title, goal, context | TEXT，复用现有文本长度限制 |
| attachments | JSON 数组，沿用已校验的附件引用 |
| window_mode | none / since_success；普通目标与连续检查 |
| required_sources | JSON 数组，每项有稳定 id、label、可选 url；记录目标中明确要求的来源 |
| cron, timezone | 五段表达式和独立 IANA 时区 |
| status | active / paused / cancelled |
| next_run_at | active 时非空，其他状态为空 |
| pause_reason | manual / invalid_schedule / attachment_unavailable；非 paused 时为空 |
| task_budget_ms | 正整数，每次 Task 的独立执行预算 |
| baseline_at | 本轮检查进度的起点；普通目标为空 |
| covered_through | 本轮成功检查终点；还未成功时为空，不用 baseline 冒充成功 |
| created_event_id, authorized_event_id | 创建与最近用户授权来源，引用 personal_events |
| created_at, updated_at | UTC 时间 |

active 的 since_success 计划必须有 baseline_at；covered_through 不早于 baseline_at。none 模式的这两个字段都为空。暂停期间变更内容可暂留空 baseline，恢复时按新定义计算。

建立 active 计划的 (next_run_at, id) 部分索引。调度推进 next_run_at 或完成检查推进 covered_through 不增加 revision，避免日常运行使用户编辑无故冲突。

内容字段为 goal、context、attachments、window_mode、required_sources。首版对这些字段的实际变化统一重设 scope，不用模型判断某个改字是否足以影响检查结论；因此纯文字背景调整也可能引起一次重复检查。title、cron、timezone 和预算不重设 scope。

### 2.2 personal_schedule_occurrences

| 字段 | 类型与含义 |
| --- | --- |
| id, schedule_id | 主键与计划外键 |
| schedule_revision, scope_revision | 创建时的版本快照 |
| scheduled_at, checked_at, coalesced_until | 原应触发时间、调度决定时间、可空的合并截止时间 |
| outcome, reason | enqueued / skipped_busy / blocked，以及原因 |
| task_id, event_id | 入队时关联的 Task 与 schedule 来源事件 |
| definition_json | 不变的内容、必需来源、模式、时区快照 |
| started_at | 首次进入执行器前写入，一旦有值便不清除 |
| window_start, window_end | since_success 任务首次开始时冻结的区间；普通目标为空 |
| coverage_eligible | 是否仍有资格为原计划推进进度，初始为真 |
| check_result_json | 与该区间有关的结构化完成审查，不覆盖已有成功凭据 |
| credited_task_revision, credited_at | 可空，记录哪次 Task 版本已被成功检查账本接纳 |

约束：

- UNIQUE(schedule_id, scheduled_at) 与 UNIQUE(task_id)、UNIQUE(event_id)，非空外键有效。
- enqueued 必须同时有 Task 和来源事件；skipped_busy/blocked 没有 Task。
- window_start/window_end 同时为空或同时有值，非空时 start < end，end 等于 started_at。
- credited_at 非空必须关联结构化完整审查及 Task revision。
- outcome 只表示调度决定，执行状态继续 JOIN Task，不创建第二套 Task 状态机。
- definition_json 和原触发时间不随计划或 Task 修改而改变。

首次开始标记保存在 occurrence 即可，只作用于关联计划的 Task，普通 Task 表无需因此增加字段。Recover 不清除它。已领取但尚未进入 BeginScheduledRun 的 Task 仍属于“未开始”。

### 2.3 personal_schedule_context_suggestions

| 字段 | 类型与含义 |
| --- | --- |
| id, revision | 建议编号和版本；重新展示修改过的候选时增加版本 |
| schedule_id, schedule_revision | 目标计划及展示时版本 |
| origin_event_id, task_id | 提出建议的回调与 Task 来源 |
| change_key | 程序生成的稳定变化身份，用于一次自动提醒 |
| memory_refs_json | 对应记忆 key、version 与从当前 memory.value 逐字摘录的依据；不是回调事件原文 |
| old_context, new_context, replacements_json | 原背景、完整改后背景及明确的局部替换 |
| status | pending / applied / dismissed |
| notified_event_id | 可空；只有实际保存通知到 outbox 时才填写 |
| presented_event_id | 最近一次展示候选的事件；应用时要求该事件已完成且有带 suggestion_id 的 outbox 差异。同一事件不能立即应用新版本 |
| created_at, updated_at | 时间 |

UNIQUE(schedule_id, change_key)。变化身份采用规范化的记忆 key、旧背景原文片段和新事实值，不含模型生成的解释、不含计划或记忆版本号；版本号分别用于确认时校验。

程序要求替换原文确实存在且位置唯一，改后背景只能包含已展示替换。多个位置匹配或无法定位时不构造可直接应用的建议，主 Agent 需先明确修改内容。

已提醒的同变化不因另一个周期、改名或预算变更再次自动推送。确认时若新事实从 B 变为 C，保留 B 的原记录并将其置为 dismissed，创建或引用 C 对应的候选；返回 C 的编号和版本供重新确认。若变化身份相同，只更新原候选版本。已 applied/dismissed 的 C 候选不会被复活。未提醒的候选可以由后续有效回调承接；未入队通知不算“提醒过”。建议过期可从版本比较得出，不增加后台刷新任务。

### 2.4 现有表和类型

- personal_events 增加 schedule_id 及 kind=schedule；该 kind 必须 completed 且 run_id=NULL，首 Run 通过自己的 event_id 引用它，保留原有 run_id 唯一索引给阶段回调。
- history 增加 schedule_id。入站事件保留匹配到的 reply_outbox_id，以便取回具体通知引用；没有可靠引用时使用显式建议编号。
- personal_requests 增加 schedule_id 与 result_json。CHECK 保证 Task 结果、Memory 结果、Schedule 结果三选一，现有 Task/Memory INSERT 使用新列默认值。
- 计划及建议工具的结果快照放在 result_json；请求重放返回原快照，不重新验证后来已删除的附件或重新执行更新。
- Event、Message、TurnInput/Output、扫描器与只读 inspection 同步扩展关联字段。outbox 复用文本和回执，并以 `suggestion_id` 标记建议通知的每个分段，不另建消息表。

## 3. 输入与方法

以下类型用于约定契约，最终名称可随现有代码组织调整。

~~~go
type ScheduleInput struct {
    Title, Goal, Context string
    Attachments []string
    Cron, Timezone string
    WindowMode string                  // none / since_success
    RequiredSources []SourceRequirement
    InitialCheckFrom *time.Time        // 用户明确指定时覆盖默认首检起点
    TaskBudgetMS *int64                 // 省略用 Store 默认；只接受用户授权设置
}

type ScheduleTrigger struct {
    ScheduleID, ScheduleRevision, ScopeRevision int64
    OccurrenceID int64
    ScheduledAt, EnqueuedAt time.Time
    StartedAt *time.Time
    Window *CheckWindow
    Timezone string
    CoverageEligible bool
}

type CheckAssessment struct {
    Complete bool
    RequiredSources []SourceAssessment // id、complete、依据或失败原因
    Limitations []string
}
~~~

TaskInput 增加可选 Trigger；它不属于普通 create_task/revise_task 的模型可写参数。普通 Task 不带 Trigger，计划的 none 模式带归属和开始时间，但 Window 为空。

SchedulePatch 使用指针表示字段是否修改；空 context/附件列表可清空，空标题、目标、规则和时区被拒绝。状态只允许 active/paused，取消用独立方法。计划预算允许用户明确修改；不会通过 Task 追加预算隐式传播。InitialCheckFrom 的重设是明确的检查范围变更，会增加 scope_revision。

~~~go
CreateSchedule(ctx, eventID, key, input, now) (Schedule, replayed, error)
UpdateSchedule(ctx, eventID, key, id, expectedRevision, patch, now) (Schedule, replayed, error)
CancelSchedule(ctx, eventID, key, id, expectedRevision, now) (Schedule, replayed, error)

Schedule(ctx, id) (*Schedule, error)
Schedules(ctx, includeCancelled, beforeID, limit) ([]Schedule, error)
ScheduleOccurrences(ctx, id, beforeID, limit) ([]ScheduleOccurrence, error)
ScheduleForTask(ctx, taskID) (*ScheduleContext, error)

DispatchDueSchedule(ctx, now) (*ScheduleOccurrence, error)
BeginScheduledRun(ctx, runID, now) (*Run, error)
ProposeScheduleContextUpdate(ctx, eventID, key, taskID, proposal, now) (Suggestion, replayed, error)
ResolveScheduleContextUpdate(ctx, eventID, key, suggestionID, expectedRevision, decision, now) (Resolution, replayed, error)
~~~

上面是方法参数顺序的伪签名。写操作 now 只从工具或 worker 取一次，不进入请求幂等 payload；不引入全局 Clock 抽象。查询分页按 ID 倒序，默认 20、最大 100。

SettleTask 追加可选 CheckAssessment，普通 Task 与 none 模式传空。仍有 coverage_eligible 资格的连续检查任务要求 complete 审查才能结案为 completed；必需来源 ID 集合须与 occurrence 快照一致，不能临时删去失败来源。用户修改单次 Task 后已失去资格的任务，按修改后的目标结案，不强迫它满足旧计划清单，也不能为原计划推进进度。模型不给出可执行的任意时间终点，Store 始终使用冻结区间。

结构校验能保证来源齐全、区间和身份正确，不能证明网页内容客观完整。主 Agent 根据阶段证据判断完成，来源不可访问、历史缺失及未解决的覆盖缺口不能声明 Complete。没有显式固定来源的检查仍需对目标完整性负责，不把来源数组为空当作自动成功。

## 4. 时间规则

采用 robfig/cron/v3 的五段 Parser 与 Next，时区通过单独字段校验后在内部绑定，禁止模型传秒字段、命令文本、描述符或内嵌时区。库接口仅提供 Next，Previous 使用下述有界查找。[官方接口文档](https://pkg.go.dev/github.com/robfig/cron/v3#Parser)

~~~text
next = Next(now)                     // 严格晚于当前时刻
first_due = next
baseline = explicit_from ?? Previous(first_due)
window_start = covered_through ?? baseline
window_end = first_execution_time
~~~

首次基准在创建或内容变更时固定；停机前未成功也不丢掉原基准。恢复自主动暂停时 baseline=resume_time、covered_through=NULL 并增加 scope_revision。故障恢复保持它们；若暂停期间改了内容而尚未设基准，则按新定义建立。

Previous 由 schedule_time.go 实现，使用 Next 查询而不另写 cron 解析器：先向前扩大时间范围找到 Next(t)<first_due 的下界，再对时间单调谓词进行二分，取得紧邻 first_due 的上一个激活时间。精度以库实际时间点为准，不能把“每天”替换成减 24 小时。查找设置明确边界；无法确定前一周期时要求明确 InitialCheckFrom，不编造区间。Next 的零值也必须处理；所选版本源码有有界年份搜索。[Next 源码](https://github.com/robfig/cron/blob/v3.0.1/spec.go)

实现时先验证 Previous 的日历及夏令时案例和查询次数，再依赖它登记任务。时间 helper 的失败是可解释的输入错误，不允许无限逐分钟向前扫描。

到期计算只需原 due、Next(due) 和 Next(now)，不枚举停机期间每个错过的时间点。Next(due)<=now 表示合并多个周期；记录合并截止 now。始终按计划时区解释日历、按 UTC 保存实际时间，DST 的跳过/重复服从解析器并设固定测试。

## 5. 创建、修改、暂停和取消事务

统一顺序：输入形状检查 -> BeginTx -> requireUserEvent(allowCompleted=true) -> 原请求查重 -> 新请求要求用户事件 running -> 当前版本与业务检查 -> 写状态和结果快照 -> commit。先查请求重放，再检查当前附件与当前版本。

CreateSchedule 校验目标、模式、来源、附件、cron、时区和预算，保存 first_due 与 baseline；授权来源均指向当前用户事件。默认普通目标用 none，连续检查用 since_success；若明确要求固定最近一天，则不能同时承诺 since_success 的全量补查，登记回执需说明实际采用的规则。

| 修改 | 事务内处理 |
| --- | --- |
| 改内容或显式重设检查起点 | revision 与 scope_revision 递增，取消未开始 Task，清空 covered_through，建立新 baseline |
| 只改 title | revision 递增；不改区间、任务与 next_run_at |
| 改 cron/timezone | revision 递增，active 时 next=Next(now)，不重置进度；已创建 Task 保留旧时间快照 |
| 改 task_budget_ms | revision 递增，只影响以后创建的 Task |
| 主动暂停 | status=paused、pause_reason=manual、next=NULL，取消未开始 Task |
| 已故障暂停又明确主动暂停 | 更新 reason=manual 并增加 revision，恢复按主动暂停处理 |
| 主动恢复 | 验证计划和材料，scope_revision 递增，baseline=now，covered=NULL，next=Next(now) |
| 故障恢复且内容未变 | 验证后清除暂停原因，保留 scope 与进度，next=Next(now) |
| 取消 | status=cancelled、next=NULL、清除暂停原因，取消未开始 Task，保留历史 |
| 无实际变化 | 保留版本和时间，仍保存幂等请求结果 |

主动恢复与内容修改同一事务时，不补暂停期间优先，起点不得早于 now。已取消计划拒绝后续更新与恢复。

取消未开始 Task 的 helper 使用 occurrence.started_at IS NULL 选取目标，更新对应 Task 和 queued/running Run 为 cancelled。它必须与计划状态修改同事务，不能调用会自己 BeginTx 的 CancelTask。已领取但还没开始的 Run 被取消后，BeginScheduledRun 返回无可执行工作。

已开始的 Task 即使重启后状态再次是 queued，也保留。请求停掉当前工作时仍显式使用现有 cancel_task，不改变普通 Task 取消语义。

## 6. 到期事务与自动暂停

DispatchDueSchedule 一次最多处理一个 active 且 next_run_at<=now 的计划，按 due/id 排序。最近授权事件必须是已 completed 的 user 事件；工具提交后、用户回复尚未完成时不能提前开工。

事务顺序：

1. 读取并校验计划及下一时间。无法解析已存规则或无下一有效时间，转为配置暂停。
2. 检查全计划各版本的忙碌工作。queued/running/awaiting_review，以及仍能推进工作的待处理回调，阻止自动触发。
3. 稳定的 waiting_input/waiting_budget 不阻止；仍在投递终结通知不应被当成可续跑回调。failed 但尚待主 Agent 判断是否续跑的回调仍算忙碌。
4. 忙碌时写 skipped_busy 并推进 next；不补建待执行 Task，不推进检查进度。
5. 校验固定附件；材料不可用则转为自动暂停。SQL 或未知程序错误回滚并返回 Runtime。
6. 插入 completed 的 EventSchedule，run_id 留空；调用事务内共享入队 helper，保存 Task、首 Run、occurrence 和不变的定义快照。
7. 来源事件补 task_id，next_run_at 推到 Next(now)，所有记录一起提交。

createTaskRunTx 只检查最终 TaskInput 并插入 Task/Run，不做来源授权、不写请求幂等记录、不提交。普通 CreateTask 保留直接用户验证；调度器使用已登记授权，不放松 CreateTask 的权限检查。

自动暂停只由 invalid_schedule 或 attachment_unavailable 引起。它在状态事务中保存一条固定短通知至 outbox，只有 active->paused 才生成。移除旧草案在 FinishStage、SettleTask、AccountEvent、ReviseTask、Recover 中按等待状态暂停计划的设计。

若材料在入队后、首次开始前不可用，BeginScheduledRun 也需核对：暂停计划并取消尚未开始的该 Task，保留检查起点；未知存储故障回滚。开始后的读取失败按该 Task 的失败处理，下一到期检查再判断计划材料。

## 7. 首次执行、结案与恢复

BeginScheduledRun 在 acquireTask 成功后调用，返回重读并补齐输入的 Run。没有 occurrence 的普通任务不新增检查逻辑。

事务内：

1. 核对 RunRunning、当前 Task revision/status，以及对应 occurrence。已取消或过时的 Run 返回空。
2. started_at 已存在时只恢复原 Trigger；不能重新读当前计划来移动旧任务区间。
3. 未开始时确认计划仍可启动、校验附件，写 started_at=now。since_success 从同 scope 计划读取 covered/baseline，冻结 window；none 不生成 window。
4. 将冻结 Trigger 写入当前 Run.Input 并提交。执行器使用返回值，不再使用 ClaimRun 时旧的内存输入。

这是进入执行的持久边界，不代表已经发出第一次模型或网络请求。事务提交后立刻崩溃，Recover 仍保留开始标记和原区间；取消操作不能把它恢复成从未开始。

后续 taskRun 从 occurrence 重建 Trigger，不能从已变更的计划读取历史时区，也不能假定最早 Run.Input 已包含首次执行时才确定的字段。修改单次 Task 的 goal/context/attachments 时将 coverage_eligible 永久置假；追加预算或仅继续不改变资格。资格为假时，Trigger 的原区间只用于历史归属，不能压过使用者修改后的目标。

Task 的首次开始和区间不因 ReviseTask 重置。现有 completed Task 可以 Revise 再执行，因此不能只按 task_id 认定一次成功检查。已接纳的成功凭据保留；之后修改该 Task 不追溯撤销原来有效的检查，也不再给新目标记进度。

SettleTask 的同一事务：

- 沿用 expected Task revision 和回调来源检查。
- 仍有检查资格的连续检查 completed 必须有完整 CheckAssessment 和已保存的阶段结果；否则拒绝完成，要求按失败/等待状态结案。
- 保存 Task 状态、摘要及审查凭据。
- coverage_eligible 为真、scope 一致、计划未 cancelled 且区间与当前未检查边界连续时，推进 covered_through；重复或较早终点为无操作。
- 已变更 scope 的旧结果只保存为该 Task 的结果，不能修改新 scope；改变检查目标的人工 Revise 也不能推进原计划。
- 同一次成功检查记账只写一次 credited_at/credited_task_revision，不能被后来回调覆盖。

用程序绑定的参数在该事务内执行等价条件更新：

~~~sql
UPDATE personal_schedules
SET covered_through = :end
WHERE id = :schedule_id
  AND scope_revision = :scope_revision
  AND window_mode = 'since_success'
  AND status <> 'cancelled'
  AND COALESCE(covered_through, baseline_at) >= :start
  AND COALESCE(covered_through, baseline_at) < :end;
~~~

执行前仍要通过 Task revision、资格、结构化审查校验。影响 0 行可能是迟到、已覆盖或已换 scope，应与读取状态解释一致；不能把未知 SQL 错误吞成成功。

Recover 沿用运行状态恢复，但保留 occurrence 的 started_at、区间、完成凭据和建议提醒记录，不暂停等待输入/预算的计划，不重建已经提交的触发记录。调度去重只保证同一触发点至多一个 Task，不能承诺崩溃后模型执行或外部发送恰好一次。

## 8. 背景建议、确认和回复关联

不增加独立扫描记忆的定时循环。主 Agent 处理现有 Task 回调时，读取该 Task 固定背景与当前相关记忆，检测候选差异，调用 propose_schedule_context_update；建议附在本次结果或失败通知。

该工具只可记录当前 Task 所属计划的 context 修改，不可改变目标、cron、来源或其他计划。存储校验回调归属、当前计划版本和具体记忆引用。建议本身没有修改计划的权限。

生成 change_key 后查已有候选：已提醒则返回原建议并禁止再次自动附加；未提醒且仍有效可承接到当前回调。背景差异的候选识别依赖模型，必须展示旧/新事实供使用者判断，不能承诺发现所有语义冲突。

CompleteEvent 发布时由程序追加建议编号、旧文、新文和适用计划，同时在同一事务保存 outbox 并填写 notified_event_id。过期确认刷新出的候选也由程序逐字追加差异，但不计作自动提醒。应用新版本前必须存在这次展示的 outbox，不能只凭模型已完成回复判断它已展示。静默回复时不能标记已提醒；失败通知也允许附带有效候选。重启或模型重复提出同项建议不会新增第二条自动提醒。

resolve_schedule_context_update 只接受 suggestion_id、expected_revision、apply/keep；不接受由模型重写的任意新背景。decision 按用户选择传入：确认同步使用 apply，由 Store 校验过期并刷新候选；只有用户明确保留原设置时才使用 keep，不能用 keep 代替过期校验：

1. 验证直接用户事件与请求幂等。
2. 加载建议，核对建议版本、指定计划及尚未处理状态。
3. 核对计划版本、原背景和记忆 key/version。过期时不修改计划；持久化新的可展示候选版本并回复最新差异，让用户重新确认；若差异已不存在则说明无需更新，不假装应用。
4. 用户明确 apply 且校验通过，在同一事务调用计划内容更新 helper，取消未开始旧 Task、重设检查 scope；建议置 applied，保存请求结果。
5. keep 置 dismissed，不改计划。同变化以后不再次自动询问。

用户主动确认时发现过期而重新展示，不与“同变化只自动提醒一次”冲突；不要用版本号当提醒去重键，也不要为保持去重而绕过版本校验。

已送达回执能定位原 Task，但原 replyTaskID 丢弃具体 outbox 分段。扩展为回复关联结构，保留 reply_outbox_id；据该通知明确出现的建议编号查持久记录，并要求该分段属于候选最近一次展示，旧通知不能悄悄指向刷新后的版本。历史只保留最近若干条且文本会截断，不能依赖它重建建议。没有唯一目标时询问编号，不批量猜测。`reply_suggestions` 只放具体回复分段对应的建议，普通待处理建议列表不能替代回复关联。

普通的 Task 补充资料、追加预算只修改该 Task。使用者明确“以后每次”可直接调用计划更新工具；无需强迫所有明确的用户修改都经过背景建议流程。

## 9. Runtime、工具、配置和查询

Runtime.Run 在 Recover 后增加一个 schedule loop，复用现有默认 200ms 空闲等待；failures 容量随 worker 数增加一。调度不占 Task 并发槽，不调用模型。Runtime.task 增加 BeginScheduledRun，并确保取消、空返回、存储错误都释放已取得的任务锁。

用户工具为 create/list/get/update/cancel_schedule 和 resolve_schedule_context_update；回调另提供只提交建议的 propose_schedule_context_update。get_schedule 同时返回待处理建议，无需另建建议列表工具。

公开工具不接受自造 source event、started_at、window_end、covered_through 或 Trigger。登记回执显示目标、时区、下次时间、每次预算及是否连续补查。自然语言明确时直接执行，只有影响目标或时间的歧义才询问。

增加 personal.timezone 及 KAGARI_PERSONAL_TIMEZONE，默认 Asia/Shanghai，配置加载时校验。默认时区由启动入口传到计划工具，Store 输入已经包含明确时区；保留 NewStore 现有默认预算参数。

Event、Message、TurnInput/Output、工具归属集合、history、publish、附件替代回复、recoverTurn 都保留 ScheduleID。本轮有成功的计划写操作（包括背景建议的记录与处理）时，回复关联使用这些操作涉及的计划，只读查询不改变关联候选；写操作涉及多个计划时不强绑一个编号。没有成功写操作时沿用查询与原事件的关联规则。模型不能通过最终输出自行伪造身份字段。

Task 回调 ScheduleID 从 occurrence 得出，EventSchedule 不进入主模型或用户记忆写入分支。Runtime.context 只加载当前关联计划和建议；其他计划由工具查询，不把所有模板塞进每个回合。

新增只读 CLI schedules [ID] 与 schedule-history ID，支持 before/limit，使用 OpenExisting 且不初始化或 Recover。日志增加 schedule_id、scope_revision、occurrence_id、suggestion_id；仍使用现有日志存储。

## 10. 文件与交付顺序

| 文件或模块 | 职责 |
| --- | --- |
| personal/schedule.go、personal/schedule_time.go | 类型、输入与时间计算 |
| personal/sqlite/schedule_schema.go | 三张新表、约束、索引 |
| personal/sqlite 的 schedules、schedule_lifecycle、schedule_queries | 用户写事务、请求快照、列表查询 |
| personal/sqlite/schedule_dispatch.go | 到期事务、忙碌判断、故障暂停 |
| personal/sqlite/schedule_execution.go | 首次执行、取消未开始 Task、检查进度记账 |
| personal/sqlite 的 schedule_suggestions、schedule_suggestion_flow | 建议持久化、去重与确认 |
| personal/assistant/schedule_tools.go、personal/service/runtime_schedules.go | 对话入口与运行循环 |
| personal/sqlite 的任务与事件事务 | 共享入队、Trigger、审查与消息关联 |
| personal、personal/assistant、personal/execution、personal/sqlite/inspection.go | 类型传播、固定背景优先、检查区间、只读查询 |
| internal/config、cmd/kagari | 默认时区、启动接线、独立 CLI FlagSet |
| tests、README、相关 V2 文档 | 调用者、接口替身、样本、使用约定 |

四个独立可验收迭代：

1. 普通周期任务：时间/schema、登记查询、调度、首次开始标记、暂停恢复取消和原 Task 执行链。
2. 连续检查：必需来源、检查区间、成功检查记账、内容变化和两种暂停恢复、人工修改资格。
3. 背景建议：候选去重、确定通知引用、过期确认和固定背景优先。
4. 整合：完整聊天入口、CLI、观测、恢复与兼容回归、文档。

主 Agent 负责 schema 与跨模块契约、审查和集成；编码阶段按届时有效的 AGENTS.md 选择 Luna 模型及思考强度，只委派边界明确的独立编码任务，不让多 Agent 同改共享文件。设计阶段只有只读核查，不委派架构决策。

## 11. 必需验证

| 组合 | 验证内容 |
| --- | --- |
| 时间 | 五段限制、默认/显式时区、严格未来、Previous、跨日/月/闰日、DST、无有效前后时间、长停机不逐次枚举 |
| 原子入队 | event/Task/Run/occurrence/next 同进同退，重启与同一时间点重复检查不重复建 Task |
| 授权 | 未完成的用户回合不触发；网页、材料、自动事件和 Task 回调不能应用计划变更 |
| 首次开始 | ClaimRun 后等待期间取消有效；Begin 后崩溃恢复保留标记和区间；开始与取消两种事务顺序 |
| 检查 | 首次迟到仍保留起点、排队终点、空结果但检查完整、部分来源失败不推进、通知失败不重查 |
| 进度 | 普通目标无 cursor、重复记账幂等、人工并行乱序完成不倒退、旧 scope 与人工改目标不推进当前进度 |
| 用户操作 | 改内容取消未开始、改名/时间保留进度、手动恢复跳过、故障恢复补查、组合修改优先级 |
| 预算与失败 | 无自动加预算、等待不暂停计划、不阻挡下一周期、每次失败通知、仅该 Task 的预算修改 |
| 建议 | 同变化一次提醒、未入队不算提醒、失败结果可附建议、过期重新展示、唯一回复关联、keep 后不再提醒 |
| 恢复兼容 | 旧库不删除、Run/Task 恢复不清元数据、普通 Task/记忆/附件/渠道行为保持、CLI 只读 |

回滚测试可在临时 SQLite 中用 trigger 抛出插入错误，验证原子性，不向生产代码增加故障注入开关。时间使用固定 now 和模拟执行器，不实际等待一天。

实现阶段按迭代运行受影响包测试；集成后运行 go test ./...、go vet ./...、go test -race ./internal/personal/... ./cmd/kagari 和 go build ./cmd/kagari。真实模型另验收少量登记、检查完整性判定、背景候选与过期确认，不把模拟测试称为真实模型验收。

2026-11-06 验收记录：四个迭代全部通过。使用 `GOCACHE=/tmp/kagari-go-cache` 完成 `go test ./... -count=1`、`go vet ./...`、`go test -race ./internal/personal ./cmd/kagari`，并以 `go build -o /tmp/kagari-schedule ./cmd/kagari` 构建成功。旧库拒绝且数据保留、CLI 只读和双方并行功能的接口回归均通过。

真实模型使用隔离临时库及虚构项目，两个 `TestLivePersonalSchedule*` 样本在同次运行中通过：普通/连续计划正确登记；人工核对回执包含时区、首次触发时间与每次预算；部分来源失败不记检查进度；模型发现 Go/Rust 背景冲突并持久化候选；记忆改为 Kotlin 后，旧确认只生成待确认的新候选，计划仍保留 Go；outbox 展示与存储一致的具体差异。运行方式为设置 `KAGARI_LIVE_EVAL=1` 和 `KAGARI_EVAL_CONFIG_DIR` 后执行 `go test ./internal/personal/... -run '^TestLivePersonalSchedule' -count=1 -v`。

验收中修正了背景证据来源与个人记忆写入证据的混淆，以及过期确认被误选为 keep 的指令歧义；定时回调指令单独要求完成背景比较和结果审查。候选识别仍依赖模型，小样本通过不代表能发现所有语义冲突。未连接真实渠道发送通知；渠道与 outbox 使用已有模拟接口验收。早期两表 DDL 的原型检查不作为本方案的验证证据。

主要验收证据：

| 范围 | 测试文件与行为 |
| --- | --- |
| 日历与时区 | `personal/schedule_time_test.go`：五段规则、日/月/闰日、DST、Previous 有界查找、无有效时间 |
| 用户操作与授权 | `personal/sqlite/schedules_test.go`：暂停/恢复、scope、内容更新、旧请求重放、拒绝非用户授权、事务回滚与分页 |
| 调度与检查 | `personal/sqlite/schedule_execution_test.go`：授权完成边界、合并漏跑、忙碌跳过、首次开始、冻结区间、来源集合、原子记账与旧库保护 |
| 运行边界 | `personal/sqlite/schedule_acceptance_test.go`：人工并行乱序完成、已接纳凭据保留、预算隔离、开始后暂停、故障与逐次失败通知 |
| 背景建议 | `personal/sqlite/schedule_suggestions_test.go`：证据引用、去重与重放、原子应用、过期刷新及回滚、二次确认、保留原设置 |
| 端到端关联 | `personal/service/runtime_schedule_test.go`：等锁期间取消、数据库重开、提交后中断恢复、精确通知分段、附件替代回复、人工改目标后的历史区间 |
| 模型与执行器 | `personal/assistant`、`personal/execution`、`personal/service` 的计划测试：工具调用、固定输入、真实模型登记与回调判定 |
| 只读查询 | `cmd/kagari/personal_test.go`：计划及触发历史查询、不建库、不恢复或改写服务工作 |


2026-11-06 上下文五迭代整合后，再次通过全仓测试、go vet、personal/CLI 竞态检查和构建。冻结检查区间、CheckAssessment 及计划进度事务保留；上述定时真实模型样本是整合前记录，本轮新增的真实上下文样本见[上下文管理实现方案](CONTEXT-MANAGEMENT-IMPLEMENTATION.md)。重新运行定时真实模型用例时，配置必须包含 `model.context_window`，也可通过 `KAGARI_MODEL_CONTEXT_WINDOW` 提供该端点实际容量。
