# Kagari 上下文管理实现方案

状态：2026-11-06，五个迭代已实现并通过集成验收。本文细化[迭代计划](CONTEXT-MANAGEMENT-PLAN.md)中的材料存储、查询与续读、模型输入容量，作为编码时的数据与接口契约。九项已确认决定保持不变。实现使用 schema 4，默认新库路径为 `data/kagari-context.db`；旧库不迁移、不删除。

## 1. 实现取舍与代码依据

采用现有 SQLite 保存新增的文本材料及其元数据，正文和引用一次提交；附件继续使用现有附件存储。读取接口统一分页，执行器的权限由运行时绑定。模型容量采用显式配置，先做确定性的选择与缩减，再按需摘要，最后检查请求是否可发送。

不增加对象存储、向量检索、通用 ContextEngine、模型路由或新的 Agent 框架。也不因持久化材料而恢复 Eino 的执行栈。

| 改造前核对的代码 | 具体改动 |
| --- | --- |
| `agent/session.go` 的 `add/read` 只保存内存来源，ID 来自 URL/backend 并在会话内去重 | 统一接入持久化回调；观察 ID 不再依赖 URL 或进程内序号 |
| `agent/engine.go` 的 `RunTaskStage`、`taskInput` | 转发正文与工具采集都先记录；初始输入和工具结果只展开有界内容，去掉转发正文的重复注入 |
| 当前 URL 由模型通过 `read_url/browse_url` 按需读取 | 不增加执行前批量抓取；已保存材料续读不触发网络调用 |
| `personal/executor.go` 复制 Engine 后绑定工具、读取记忆 | 同处绑定 Run 身份与来源保存回调；共享 Engine 上不保存某个 Run 的可变状态 |
| `StageResult.Sources` 当前为完整 `domain.Source` | 改为持久引用；`domain.Source` 仍用于采集和进程内处理，不以清空 Content 伪造引用 |
| Store 使用单连接、事务、revision 检查；`Recover` 重排同一 Run | 在这些规则内保存材料；重跑产生新观察，原观察保留 |
| `get_run/list_runs`、`read_attachment/read_file/ls` 可返回大内容 | 模型工具改成有界视图；内部 Store 查询不因分页丢失数据 |
| `Message` 有 EventID，没有 `personal_history.id` | 增加 HistoryID，分别定位输入事件和同一事件产生的历史回复 |
| `config.Model` 只有 MaxOutputTokens，没有窗口容量 | 新增 ContextWindow；初始化所有模型调用入口时传入同一容量规则 |
| Eino 固定为 `v0.9.21`，当前 Agent 未挂载上下文 handlers | 使用 typed state hook 和 summarization；不用默认 160,000 阈值或字符数除四的计数 |

## 2. 材料存储契约

### 2.1 内容、身份与引用

材料指已经取得、需要按原内容回查的文本。首批保存网页提取正文、浏览器观察正文、转发正文；运行内压缩再接入无法从业务记录重建的工具记录。

- `material_id` 使用 `m_` 加 16 字节随机数的十六进制表示，由服务端通过 `crypto/rand` 生成。
- `source_id` 直接使用对应的 `material_id`。引用同时携带 `run_id` 便于展示出处；读取时只需 material ID，服务端查询归属和权限。
- 相同 URL 的新获取产生新 material ID。HTTP 会话缓存命中返回原引用；增加 `read_url.refresh`，显式绕过缓存的获取消耗一次网络读取额度并产生新引用。浏览器新观察继续逐次保存。
- 正文保存提取后的 UTF-8 文本，不声称保存了原始 HTML、响应字节或浏览器状态。`source_truncated` 表示采集自身不完整，与分页的 `has_more` 无关。
- 同一引用永久对应同一正文、元数据和采集时间；不原地刷新。内容哈希用于校验版本，不按哈希跨 Task 合并材料。
- 不复制附件正文进入材料表。每次读取附件仍检查 available/deleting/deleted 状态，删除后不能从另一材料入口读出原件。
- 工作区文件继续是可变的编辑位置，不自动为每次读取制造历史副本；需要固定交付版本时使用现有不可变附件。

`domain.Source` 保持采集类型，`Usable()` 仍检查正文和状态。新增 `domain.SourceRef`，包含 ID、RunID、URL、Title、Status、FetchedAt、SourceTruncated、TotalRunes，不含 Content 和完整 Links。`personal.StageResult.Sources` 改为 `[]domain.SourceRef`。`domain.Result` 可以继续在进程内保留完整来源，供现有阶段摘要等逻辑使用；保存 Run.Result 前转换为引用。

CLI 要导出完整来源时，显式调用 Store 展开引用；不能继续假设新 Run.Result 自带所有正文。诊断读取本身不限制为模型页大小。

### 2.2 表结构

下列是增量 DDL，不是完整建库脚本。材料表在迭代一落地，`tool_record` 写入在迭代四启用。关联表和续读表与对应工具一起交付，不预建后续迭代的状态表。

```sql
CREATE TABLE personal_materials (
    seq INTEGER PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    event_id INTEGER NOT NULL REFERENCES personal_events(id),
    run_id INTEGER REFERENCES personal_runs(id),
    task_id INTEGER REFERENCES personal_tasks(id),
    kind TEXT NOT NULL CHECK(kind IN ('source', 'tool_record')),
    capture_key TEXT NOT NULL,
    body TEXT NOT NULL,
    metadata TEXT NOT NULL CHECK(json_valid(metadata)),
    content_sha256 TEXT NOT NULL CHECK(length(content_sha256) = 64),
    byte_count INTEGER NOT NULL CHECK(byte_count >= 0 AND byte_count <= 41943040),
    char_count INTEGER NOT NULL CHECK(char_count >= 0 AND char_count <= 10485760),
    collected_at TEXT NOT NULL,
    CHECK(byte_count = length(CAST(body AS BLOB))),
    CHECK(kind <> 'source' OR (run_id IS NOT NULL AND task_id IS NOT NULL))
);
CREATE UNIQUE INDEX personal_materials_capture
    ON personal_materials(event_id, COALESCE(run_id, 0), capture_key);
CREATE INDEX personal_materials_run ON personal_materials(run_id, seq);
CREATE INDEX personal_materials_task ON personal_materials(task_id, run_id);

CREATE TABLE personal_task_run_refs (
    task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
    revision INTEGER NOT NULL CHECK(revision > 0),
    source_run_id INTEGER NOT NULL REFERENCES personal_runs(id),
    authorized_event_id INTEGER NOT NULL REFERENCES personal_events(id),
    PRIMARY KEY(task_id, revision, source_run_id)
);

CREATE TABLE personal_read_checkpoints (
    task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
    revision INTEGER NOT NULL CHECK(revision > 0),
    resource_kind TEXT NOT NULL CHECK(resource_kind IN ('material', 'attachment', 'file')),
    resource_id TEXT NOT NULL,
    version TEXT NOT NULL,
    next_offset INTEGER NOT NULL CHECK(next_offset >= 0),
    note TEXT NOT NULL,
    run_id INTEGER NOT NULL REFERENCES personal_runs(id),
    updated_at TEXT NOT NULL,
    PRIMARY KEY(task_id, revision, resource_kind, resource_id, version)
);
```

材料 metadata 采用固定 Go 类型序列化，含 `format_version: 1` 和来源的 RequestedURL、URL、Kind、Title、Author、PublishedAt、Links、Status、Reason、ReadMethod、Truncated；不重复正文和 ID。未知 format_version 返回明确错误。采集时间以 UTC 保存，正文哈希按原 UTF-8 字节计算。Go 层验证 UTF-8、码点数、元数据字段与上限，不依赖 SQLite 的 TEXT length 计算含 NUL 文本的码点数。

文本上限沿用 `Prepare` 已有的 10 Mi 码点边界，对应最多 40 MiB UTF-8，避免存储层无声缩小现有输入范围；每条 metadata 上限 64 KiB。HTTP/浏览器自身更小的采集限制继续有效。超过上限返回 `material_too_large`，不能静默截断后声称已保存完整材料。默认读取正文页不需要先查询整条大正文：SQLite `substr` 与 Go 分页须处理 NUL 的一致性，首版可在现有上限内读取正文后按 Go rune 切页，后续有实测瓶颈再改变存储布局。

### 2.3 写入顺序与失败行为

`TaskExecutor → 注入 RecordSource → readingSession.add/read → Store 保存 → 来源引用与首页 → 模型`。

1. 每个实际采集操作开始时分配 capture key。网络或浏览器调用在数据库事务外执行。
2. 采集完成后规范化来源，检查正文、元数据和状态。读取失败但取得的失败原因、部分正文也保存为观察，保留 failed/incomplete 等状态；“记录已保存”不等于“读取成功”。
3. Store 开事务，核对 Run 的 Task、Event、revision 和当前可执行状态。正文和元数据在同一 INSERT 中提交。身份来自运行时绑定，不接受模型传入的归属字段。
4. 同一 capture key 的重复提交，仅当归属、类型、正文、元数据、采集时间完全一致时返回原引用；不一致返回 `capture_conflict`。不能用 INSERT OR IGNORE 吞掉冲突。
5. 提交成功后才更新 session 可见缓存并返回引用。磁盘满、SQL 错误、事务提交失败属于执行错误，停止本阶段；不包装成“网页打不开，请换一个 URL”。

崩溃发生在提交前：没有成功返回的持久引用。提交后、工具返回前崩溃：`list_materials(run_id)` 可以找到材料，不依赖 Run.Result 已写入。恢复仍重排同一 Run；新进程的新采集使用新 capture key，不覆盖上一进程的观察。没有执行位置恢复，也不承诺外部工具恰好执行一次。

材料不提供模型侧修改、删除工具；保存期限随 Task 历史。只有上下文窗口中的展开内容被压缩。当前 Store 单连接，事务内 helper 接收 `*sql.Tx`，不嵌套调用另一个会 BeginTx 的 Store 方法；读 Rows 必须关闭后再进行后续查询或文件操作。

### 2.4 Go 边界与跨 Task 关联

沿用现有 Repository，添加有明确职责的 Store 方法；不再套一层通用存储接口。以下签名表示契约，分页 DTO 在下一节定义：

```go
// SourceRef 位于 domain；ReadScope 等 DTO 位于 personal。
type RecordSource func(context.Context, string, domain.Source) (domain.SourceRef, error)

// RunTaskStage 新增必需的 RecordSource 参数；生产调用不能省略持久化。
// string 参数是 session 在采集前分配的 captureKey；同一次保存重试复用它。
SaveSource(ctx context.Context, runID int64, captureKey string, src domain.Source) (domain.SourceRef, error)
ListMaterials(ctx context.Context, scope ReadScope, runID int64, page ListPage) (MaterialPage, error)
ReadMaterial(ctx context.Context, scope ReadScope, id, part string, page TextPageRequest) (TextPage, error)
SearchMaterial(ctx context.Context, scope ReadScope, id, query string, page SearchPage) (MatchPage, error)
SaveReadCheckpoint(ctx context.Context, runID int64, update ReadCheckpointUpdate) (ReadCheckpoint, error)
```

`internal/agent` 只知道 RecordSource 和来源类型，不导入 personal。测试使用显式的内存 recorder；不存在“回调为 nil 就悄悄跳过保存”的生产路径。缓存只收录保存成功的来源。读取失败的来源和存储失败使用可区分的错误类型。

`ReadScope` 由主 Agent/执行器工具构造函数绑定，含当前 EventID 或 RunID，恰有一种有效身份；不出现在模型工具参数中。主 Agent 读取本使用者的记录。执行器每次调用沿用 `checkTaskRun` 的状态检查，只能读本 Task 的历史材料及本 revision 明确关联的外部 Run/附件。

在 create_task/revise_task 增加 `source_run_ids`，与 Task 修改、revision 和 Run.Input 保存同事务提交 `personal_task_run_refs`；TaskInput 保存排序、去重后的关联快照。省略 revise 参数表示继承，显式空数组表示清空。使用已有 Task 写操作的 request key 重放保护，不增加单独的授权修改工具或另一套 personal_requests 结果类型。

跨 Task 首版只允许关联已终止的 Task Run（completed/failed/cancelled），避免关联后可读取的证据集合继续增长。只授予该 Run 的结果、来源和明确产物的读取；不授予旧 Input、旧 Task 全部历史、个人记忆、内部工具记录或它传递关联的其他 Run。附件依旧需要本 Task 的显式 attachment 关联。模型不能通过读到另一个 ID 扩大权限。主 Agent 的任务回调不能自行新增外部关联；新增关联由用户事件下的主 Agent 操作。

## 3. 查询、分页与续读

### 3.1 公共返回规则

```go
type TextPageRequest struct {
    Offset  int    `json:"offset"`            // Unicode 码点，含起点。
    Limit   int    `json:"limit,omitempty"`   // 0 使用默认值。
    Version string `json:"version,omitempty"`
}

type TextPage struct {
    Text            string `json:"text"`
    Offset          int    `json:"offset"`
    End             int    `json:"end"`       // 不含终点。
    TotalRunes      int    `json:"total_runes"`
    NextOffset      *int   `json:"next_offset,omitempty"`
    HasMore         bool   `json:"has_more"`
    Version         string `json:"version"`
    SourceTruncated bool   `json:"source_truncated"`
    // 外层附带完整资源引用、来源状态和采集时间。
}
```

- 文本默认 4,000、最多 8,000 码点；列表默认 20、最多 50 项。负数、超过上限等参数返回 `invalid_page`，不默默钳制。offset 等于总长返回空末页，大于总长报错。
- 单次工具完整 JSON 编码后不得超过 32 KiB，包含元数据、链接、外层结构和转义开销。先缩小文本/条目数，再序列化并复核；End、NextOffset 按实际返回内容重算。禁止切断 JSON 字符串、ID 或路径。连一个完整必要引用都装不下时返回 `response_item_too_large`。
- 接入容量检查后，工具实际整包上限取 `min(32 KiB, floor(H/4))` 字节，给规范输入和其他消息留出空间；码点 limit 是期望的最多数量，不保证每次读满。整个请求仍须重新计数，这个单工具上限不替代最终硬检查。
- 摘要卡片可缩短 title、description 等展示字段，并标记 `omitted_fields` 及展开入口；不能缩短身份、版本和权限信息。所有长字符串包括 error/reason 都要遵守同一响应上限。
- `version` 对不可变材料为正文 SHA-256；读取 metadata 时对其规范 JSON 字节另算版本。对工作区文件为本次完整文件字节的 SHA-256。
- JSON 存档分页统一作为 text 字符串返回，metadata 与 Run.Input/Result 均如此；一页不是可独立解析的原始 JSON 对象，外层分页响应始终是有效 JSON。
- 工具结果是材料而非指令。任何读取结果均不能改写 Task 要求或扩大工具权限。

例如返回 `offset=0,end=4000,total_runes=19000,has_more=true,source_truncated=true`，只表示保存了 19,000 码点中的前 4,000 个供本次阅读；保存的 19,000 码点本身还可能是原网页的截断内容。到末页后 `has_more=false`，`source_truncated` 仍为 true。

### 3.2 模型工具清单

保留现有工具名称，对新增参数使用可选字段；默认行为改为有界返回，需要同时更新提示词、schema 与模拟模型 fixture。

| 工具 | 参数与返回 | 权限及边界 |
| --- | --- | --- |
| `list_tasks` | query、updated_from/to、before_id、snapshot_max_id、query_key、limit；摘要卡片及游标 | 主 Agent；关键词查 title/goal/context/summary |
| `get_task` | task_id；状态、revision、有界进展和字段读取入口 | 不再把任意长 Context 隐式全量注入 |
| `read_task` | task_id、section=goal/context/checkpoint、文本分页 | 当前要求的规范字段；主 Agent或本 Task |
| `get_event` / `get_message` | event_id / history_id，文本分页 | 分别读取原始事件与具体历史消息，不能只靠 event_id 区分多条回复 |
| `list_runs` | task_id、before_id、snapshot_max_id、query_key、limit；Run 摘要 | 执行器本 Task；外部关联不开放旧 Task 的整张列表 |
| `get_run` | run_id；状态、阶段摘要、材料索引入口 | 主 Agent、本 Task或明确关联的外部 Run |
| `read_run` | run_id、section=input/result、文本分页 | 外部 Run 只开放 result；JSON 存档片段放在 text 字段中，不冒充独立有效 JSON 对象 |
| `list_materials` | run_id、before_id、snapshot_max_id、query_key、limit；来源元数据卡片 | 在阶段结束前也可查询；外部 Run 仅返回可授权的来源 |
| `read_material` | material_id、part=body/metadata、文本分页 | 不触发重新采集；metadata 包含完整来源字段和 Links |
| `search_material` | material_id、query、offset、limit；命中区间和短片段 | 只搜索已保存正文，不联网，不代表全文阅读 |
| `read_attachment` | 原 ID 加文本分页 | 保留 UTF-8、文本大小及每次删除检查 |
| `read_file` / `ls` | 原路径加 offset/version 或 after_name/version；文本页/目录页 | 工作区路径边界与文件大小上限不变 |
| `read_url` / `browse_url` | 原参数；read_url 增加 refresh；返回引用、来源卡片和正文首页 | 实际获取受原 max_sources 限制；材料续读不计入网络次数 |
| `checkpoint_reading` / `read_checkpoints` | 资源引用、version、expected_offset、next_offset、note；阅读记录 JSON 文本分页 | 仅当前执行器；保存和回查已报告的阅读结论与续读位置 |
| `get_progress` / `read_progress` | task_id、history、列表游标；seq 对应条目的 JSON 文本分页 | 本 Task 或主 Agent；默认最新条目，history=true 保留每次修改的原记录 |
| `submit_progress` / `correct_progress` | 条目增量和证据；纠正附 revision、expected_version | 执行器暂存增量，FinishStage 原子保存；主 Agent 纠正保留旧版本 |
| `get_task_input` / `report_task_inputs` | 当前 Run 分配的 event_id 文本页；已处理 event_ids | 查看不等于处理；成功 FinishStage 才标为 applied |

主 Agent 的文件工具额外接受 task_id，执行器的 task_id 由工具闭包绑定。主 Agent 使用新增 `workspace.OpenExisting` 只读打开工作区，复用 os.Root 边界，不调用会 MkdirAll 的 Open。主 Agent 不因此获得 write_file/edit_file。

`get_run`、`get_task` 等摘要自身也要遵守整包限制；索引只返回有限项和分页入口，不能在“摘要”里重新塞入全部来源。CLI/Store 内部完整读取与模型视图分开。

### 3.3 查询与分页一致性

Task、Run 列表按 ID 降序用 keyset 分页，首屏记录 snapshot_max_id；后续查询 `id <= snapshot_max_id AND id < before_id`。材料 ID 为随机字符串，不能充当时间高水位；材料列表使用表中的 seq 作为整数游标，按相同规则降序翻页。seq 仅用于列表顺序，材料引用始终使用 id；后页同时传回 next_before_id、snapshot_max_id、query_key，改变查询条件时从首屏开始。

Task 的状态、摘要仍可能在翻页时改变；高水位只排除新增行，不承诺多次请求看到同一个数据库快照。时间条件为 UTC 的 `[updated_from, updated_to)`，与关键词条件取交集。关键词长度最多 256 码点，首版使用参数化 `instr` 做字面匹配，不提供正则、SQL 通配符或语义排序；中文可直接匹配，英文大小写按字面区分。状态过滤变化造成的候选差异由工具说明，不保存长事务。

`search_material` 使用 Go 字面子串匹配，大小写敏感，返回码点 offset/end。默认最多 20 个命中，最多 50；每处上下文不超过 160 码点，同时服从 32 KiB 整包限制。游标是下一个未检查的正文码点位置，允许跨页邻接匹配，不能因分页漏掉跨界关键词。匹配后从命中起点的下一个码点继续，支持重叠命中。空 query 拒绝；无命中明确返回末尾。

对材料、附件等不可变资源，offset 可直接定位。工作区文件 offset>0 必须携带 version，且每次先读取并校验完整文件；版本变化返回 `version_changed` 和新版本，要求从 offset=0 重新读取。目录页按名称排序，对完整条目列表计算版本；后页必须携带版本，目录变化同样重新开始。读取到的版本不代表此后编辑的原子前置条件；本轮不把 edit_file 改成另一个并发编辑协议。

仍可能变化的 Task 字段、尚未结束的 Run.Result 同样要求后页携带字段内容版本；Task 版本还包含 revision。历史事件和具体 history 消息按已保存记录读取。不能把“数据库中的文本”一概当成不可变内容。

### 3.4 续读位置不等于处理完成

工具返回一页后不自动推进持久续读位置。模型尚未处理该页就崩溃时，自动跳过它会丢失工作。

执行器通过 `checkpoint_reading` 主动提交：资源及版本、连续读到的位置、已提取要点和下一步。note 必填，最多 2,000 码点。服务端检查当前 Task/revision、资源权限、版本和范围，使用 expected_offset 比较并交换；相同位置与 note 的重试返回原结果，冲突返回 `checkpoint_conflict`。新记录 expected_offset 为 0。

运行内工具记录已经成功返回的区间；新 next_offset 不得跨过未返回的区间。上次已提交的连续前缀与本轮已返回区间合并校验，再保存位置与 note。该校验只说明材料曾交给模型，note 是模型报告的结论，不证明模型理解正确。关键词搜索的短命中片段不能冒充中间全文已读。

下一 Run 默认呈现该 revision 的 checkpoint 与 note，再由模型决定从该处继续或重新阅读；恢复同一 Run 也如此。revision 改变后保留旧记录，但不自动沿用阅读完成声明。附件删除或文件版本变化使对应续读入口失效，返回明确原因。

长文需要超过当前 max_iterations 才能读完时，在本阶段结束前保存 note 和位置，并报告尚未完成；后续仍由主 Agent 调用 continue_task。不自动提高网络读取、工具往返、时长或模型迭代预算。计量分别记录网络尝试次数、分页调用次数、返回区间和已提交阅读前缀。

## 4. 模型输入容量规则

### 4.1 配置与计算

新增 `model.context_window`，表示端点实际支持的输入加输出总 token 容量，部署者必须填写；不根据任意模型别名猜测。`max_output_tokens` 保留现有含义和默认值。只有需要调用模型的 `Validate(modelRequired=true, ...)` 要求窗口配置有效，inspect 等诊断命令不能被此要求阻塞。

示例配置留 `context_window: 0` 并注释必须按端点填写；0 不表示无限或自动推断。启动错误直接指出所缺字段。下列比例是统一的首版工程策略，不增加五个可调配置项：

```text
W = context_window
O = 本次 max_output_tokens
R = max(1024, ceil(W × 0.05))       # 安全余量
H = W - O - R                     # 可发送的输入上限
S = floor(H × 0.80)               # 压缩触发值
T = floor(H × 0.60)               # 压缩目标值
```

使用 int64 和整数运算，验证 W、O 为正，O<W，H≥4096，并避免乘法溢出。覆盖输出额度的调用按自己的 O 重算。例：W=32768、O=5000，R=1639、H=26129、S=20903、T=15677。

模型本身另有限制最大输出时，部署配置还须遵守该限制；本方案不声称单个 context_window 能描述全部提供方限制。

### 4.2 统计口径与接入点

计数覆盖实际发送的系统提示、人格、Profile、规范任务要求、消息、工具调用参数、工具返回、完整工具 JSON schema、结构化输出 schema 及输出格式指令。缓存输入仍占窗口。不能只统计用户文本或只数 content 字段。

当前可配置任意兼容端点，首版不新增 tokenizer 依赖：把待发送文本及工具/输出 schema 的规范 JSON 表示按 UTF-8 字节数计为估算 token，并记录 `count_basis=utf8_bytes`。这是有意保守的估算，不是提供方精确 tokenizer，也不是对所有协议开销的数学保证；安全余量单独扣除。实际 usage 记录 input tokens 与估算值的偏差，首版不自动根据几次调用放宽上限。未来接入非文本内容必须增加对应计数规则，不能按零成本放过。

新增一个小的 `internal/inputbudget` 包，集中 Limits、计数、错误和模型硬检查，供 agent 与 personal 共同使用，不把整个上下文选择迁入它。以 `model.AgenticModel` 包装器同时实现 Generate/Stream，在发送前合并实际 model options 中的工具与输出格式参数再计算；当前 Responses 专用格式选项由创建它的调用点同步提供计数所需的 schema，不能把不可检查的 provider option 当作零成本。共享实例上不存 Run 可变状态。

硬检查包在现有 normalizedResponsesModel 外层，保留流式事件规范化。模型替身也经过同一检查，避免生产和测试走不同容量路径。Eino `BeforeModelRewriteState` 负责组装、移除失效记忆和缩减；最终模型包装器复核其结果，防止后续 model options 或其他调用入口漏算。

| 模型调用入口 | 接入方式 |
| --- | --- |
| `MainAgent.Think` | 上下文组装与压缩 handler；最终硬检查 |
| `Engine.RunTaskStage` | Run 绑定的选择、记忆有效性、材料缩减与压缩 handler；最终硬检查 |
| `MainAgent.ReviewMemory` | 来源与候选记忆明确分区；硬检查；过大则失败，不摘要改写待提取的原始证据 |
| `formatTurn` | 包含输出 schema 的硬检查；失败不能重跑已执行的任务/记忆写操作 |
| 摘要模型 | 同一硬检查、独立输出额度，无工具、无递归压缩 |

当前源文件里的 rune 限制仍用于入口校验或单页限制；它们不再承担“保证整个请求适配模型”的职责。

### 4.3 组装、缩减与失败顺序

每次模型调用执行以下顺序：

1. 读取规范 Task 输入和控制状态，校验 revision；移除已遗忘的记忆块及对应直接工具结果。普通更正沿用本 Run 固定版本，下个 Run 更新。显式 Task 要求不等待 Memory Review。
2. 按用途选择内容。固定保留系统指令、当前请求/Goal/Context/Step、revision、计划的冻结检查区间、当前控制状态和读取入口。当前用户消息若混有长材料，原始要求保持完整，已被明确作为附件/转发材料的正文才改成引用；不擅自把整条用户要求降级为材料。
3. 累计进展、当前阶段结果、相关证据、选择的记忆与近期交流按计划优先级加入。先保留必需部分，再分配剩余额度，不给每类信息永久保留一大块空预算。省略项记录原因和读取入口。
4. 请求估算达到 S 时，先将早期大工具结果替换为短说明和可读取引用；只处理已经持久保存或能从规范业务记录重读的内容。保留最新完整的工具调用/返回组，不割裂配对。先缩减到 T，达标后不再调用摘要模型。
5. 仍达到 S 且有可压缩历史时，调用一次摘要模型。输出最多 `min(2048, max_output_tokens)` tokens；输入只取可压缩的完整历史前缀，摘要请求本身也要装得下。其余记录保留引用，不将整个超长请求转发给摘要模型。
6. 校验摘要非空、引用可解析、替换后保留必需部分且确实缩短，再替换窗口前缀并重新统计。程序不能证明摘要语义完全正确；目标和约束始终从规范字段重新加入。记忆正文不交给通用历史摘要持久复制，避免遗忘后仍从直接副本注入；既有分析仍按已确认决定保留。
7. 最终估算≤H 才请求主模型。超过 H 返回 `ContextLimitError`，保存已取得材料和阶段部分结果，不截掉当前要求、不反复重发相同请求。

无可压缩历史时，只要请求≤H 就可发送，即使超过 S；S 是触发检查值，不是另一条拒绝线。摘要没有达到 T 但已低于 H 时可以继续。相同历史前缀只尝试一次摘要，只有新增可压缩内容后才重新触发；不配置额外重试或 failover 模型。

摘要失败或无效时保留原历史；此前成功的确定性结果缩减仍有效。该状态≤H 则继续，>H 则明确结束阶段。压缩耗时计入现有 Task/Run 或主事件的超时预算，不新增旁路执行额度。提供方实际返回上下文超限时同样停止该次调用，不自动多次缩短重试。

错误至少区分 `required_input_exceeds_limit`、`tools_schema_exceeds_limit`、`history_exceeds_limit`、`unsupported_content_count`，附估算值、H、计数口径和大项统计；不能把容量失败包装成用户任务已经完成。工具参数错误可作为有界工具反馈让模型修正，存储失败和不可发送的模型请求则向运行时传播。

### 4.4 Eino 与持久记录

复用 `adk.TypedChatModelAgentConfig.Handlers` 和 `BeforeModelRewriteState`。摘要使用现有 `summarization.TypedConfig[*schema.AgenticMessage]` 的 TokenCounter、Trigger、GenModelInput、Finalize：明确传入上述计数与阈值，控制摘要输入前缀，校验后保留近期消息。

材料和业务记录已经有读取入口，不为了使用 reduction 的文件接口再造文件系统适配层；早期结果替换在同一 handler 内完成。Eino reduction 的默认 `/tmp`、read_file 入口和 nil backend 清除行为不符合本契约，不直接启用。不能让压缩后的引用指向普通可编辑工作区或轮转日志。

需要保存不可重建的工具参数/结果时，写入同一材料表的 `tool_record`，metadata 保存 tool_call_id、工具名和记录类型；与当前 Event/Run 绑定。持久化成功后才替换窗口内容。附件和记忆工具返回使用类型化引用，重新展开时检查当前有效性，不把它们原文藏进 tool_record 绕过删除或遗忘。历史消息和 Run 内容优先使用既有 ID，不重复存档。

迭代二给 Run 增加 `memory_versions` JSON 字段，仅保存已选 key 与版本号。同一 Run 重启后仍从记忆历史读取该版本；冻结版本之后出现遗忘记录时不再返回旧值，即使随后重新保存同一 key。新 Run 重新选择。该字段不复制记忆正文。

Run 的 `context_manifest` JSON 字段含格式版本、revision、关联来源及版本、选择/省略原因、各部分实际加入长度、组装时间和容量参数。首次模型调用前保存；失败不启动模型。后续工具读取、版本检查和压缩调整沿用 trace，区分“初始选择”与“整个 Run 实际访问”。主对话只写 trace，不新增对话快照表。

trace 增加 `context.assembled`、`context.reduced`、`context.limit`，记录估算、实际 usage、读取区间、材料引用与压缩结果；不为了指标复制一份记忆正文。trace 有轮转，不承担材料耐久性或逐字重放承诺。

## 5. 与现有状态事务和并行改造的衔接

- `decodeTaskInput` 使用 DisallowUnknownFields，新增 source_run_ids/memory_refs 必须同步 Go 类型、创建/修改/继续路径和 fixture，不能只在工具 schema 加字段。保留定时计划正在加入的 Trigger 字段。
- 定时任务实际开始时由 `BeginScheduledRun` 冻结检查区间并返回更新的 Run；上下文组装使用它返回的新 Input，不能使用 ClaimRun 时的旧副本。固定背景与记忆冲突仍按定时计划设计处理，不自动改写计划背景。
- 保留已确认的定时连续检查 `CheckAssessment` 要求；“不增加通用 Task 验收门槛”不撤销该专用完成规则。新输入阻止 completed 的检查与现有完成事务整合。
- 材料保存独立于 FinishStage；累计进展仍在迭代三由 FinishStage 原子应用增量。读取 checkpoint 只是阅读位置和 note，不得提前更改 Task 完成状态或把用户反馈标为 applied。
- 当前工作树正在实现定时计划，schema 已出现版本 3。上下文改造使用集成时分配的下一版本，不把另一条改造的编号覆盖回去；上线仍采用新库路径、保留旧库及其工作区，不做数据迁移或自动删除。

## 6. 编码批次与验证

五个迭代的范围不变，以下是对应的可交付编码批次；后续批次不作为前一批必须同时完成的依赖。

| 批次 | 文件与调用方 | 交付边界 |
| --- | --- | --- |
| 1A 材料保存 | 新增 `personal/store_materials.go` 及 schema；`domain/types.go`、`personal/types.go`、`agent/session.go`、`agent/engine.go`、`personal/executor.go` | 逐次保存、不可变引用、失败传播；StageResult 改引用，CLI 导出显式展开；阶段内中断可发现材料 |
| 1B 查询与续读 | 从 `personal/tools.go` 拆查询工具；`store_events.go`、`store_tasks.go`；`attachment_tools.go`、`executor_workspace.go`、`workspace` | 有界列表/全文页/搜索，跨 Task 关联，阅读 checkpoint；更新两类提示词及模型 fixture |
| 2 组装与容量 | `personal/context.go`、`runtime_conversation.go`、`model.go`、`model_result.go`、`model_format.go`、`memory_model.go`、`executor.go`；`inputbudget`、config、CLI 装配 | Memory 版本与遗忘、manifest；收拢重复裁剪；所有模型入口硬检查；尚无摘要时超限明确停止 |
| 3 累计交接 | `store_tasks.go`、Run 结果与进展工具、上下文组装 | 复用稳定材料引用和阅读 note，按原计划原子更新累计进展 |
| 4 运行内压缩 | `agent`/`personal` handlers、inputbudget、Store 工具记录与 logging | 确定性缩减、Eino 摘要、失败路径；不重放模型执行栈 |
| 5 新输入交接 | 输入关联 Store、`runtime_tasks.go`、完成事务 | 待处理输入交接与 completed 并发检查，按原计划交付 |

配置同步范围包括 config 类型、默认值/Validate、config.example.yaml、README/V2 运行说明及 cmd 装配。类型同步范围包括 Repository 的测试替身、StageResult fixture、来源摘要/通知/诊断导出，以及 RunTaskStage 的所有调用点。不能只改主流程而让 live-eval 绕过 recorder 或预算检查。

编码时至少覆盖下列行为，不以提示词字符串断言代替状态验证：

| 范围 | 必须通过的用例 |
| --- | --- |
| 保存 | 提交前失败无成功引用；提交后崩溃可发现；同 key 相同内容幂等、不同内容冲突；同 URL 新观察不覆盖；失败观察保留状态；旧 revision 写入拒绝 |
| 权限 | 本 Task 历史可读；未关联外部材料拒绝；关联仅开放指定终止 Run 结果和来源；关联不传递；旧 Input/工具记录拒绝；回调不能扩大关联 |
| 分页 | 中英文、emoji、NUL、JSON 转义；空/末页/越界；整包 32 KiB；长标题和单项过大；无重复漏字；材料分页不产生网络读取 |
| 文件与附件 | 不存在的工作区只读访问不创建目录；文件/目录变更使后页失败；附件删除使后页失效；原路径边界与大小校验保留 |
| 查询与续读 | 超过 50 个 Task 后可找到旧任务；列表插入与状态变化行为明确；关键词跨界/重叠；工具返回后崩溃不自动前进；checkpoint CAS 与跨 Run note 保留 |
| 容量 | system/tools/格式 schema 合计超限；UTF-8 估算与边界整数运算；Generate/Stream 均拦截；所有五类模型入口覆盖；配置缺失仅阻塞模型运行 |
| 压缩 | 不割裂工具配对；摘要只一次且自身有界；空摘要/超时保留原记录；仍超 H 则停止；不把附件/记忆复制成绕过删除的材料；格式化失败不重做写操作 |
| 集成 | 定时 Run 用最新冻结 Input；专用 CheckAssessment 保留；新库拒绝旧 schema；旧文件保留；恢复不声称执行恰好一次 |

实现后按改动运行 `go test ./internal/personal ./internal/agent ./internal/workspace ./internal/config ./internal/inputbudget`；事务与状态改动跑 personal 的 race 检查，再完成仓库完整检查。真实端点验收记录模型窗口配置、实际 usage、估算偏差、分页次数、摘要次数、任务质量与耗时，不预先承诺 token 节省比例。

设计阶段曾将本文 DDL 原样放入内存 SQLite，配合最小父表验证外键、重复 capture key（包括 run_id 为 NULL）、元数据 JSON、字节长度、seq 高水位分页、revision 关联及续读位置 CAS 原型；已用 Python 原型验证中文/emoji/NUL/JSON 转义的无损分页、整包上限与容量公式示例，并检查两份设计文档的相对链接和空白。当时尚未运行新 Go 实现、真实模型或完整应用事务测试；这些原型检查不作为运行时实现的验收证据。


## 7. 实现与验收记录（2026-11-06）

本轮在 `context-feat` 完成全部五个迭代，保留定时计划和并行回复改动。数据库统一为 schema 4、24 张 personal 表，默认路径 `data/kagari-context.db`；旧库拒绝写入且保留原文件。模型运行必须显式配置 `model.context_window`，只读 CLI 不因此要求模型凭证或容量配置。

| 迭代 | 实现入口 | 验收证据 |
| --- | --- | --- |
| 1 材料与回查 | `store_materials.go`、`store_context_queries.go`、`context_tools.go`、`textpage`、`workspace/pages.go` | 材料提交后恢复、capture 冲突、窄授权与非传递关联、Unicode/NUL、重叠搜索、51 个旧任务查询、文件版本变化、附件删除、checkpoint CAS |
| 2 组装与来源 | `context_assembly.go`、`executor_context.go`、`context_memory.go`、`store_run_memory.go` | 保留当前要求和具体回复对象、整体 schema 计数、必需内容超限拒绝、初始 manifest 先保存、普通更正冻结、同 Run 恢复、forget 后重新保存仍不返回旧值 |
| 3 累计进展 | `store_progress.go`、`progress_tools.go` | 跨阶段累计、历史版本与高水位分页、正文回查、纠正 CAS、证据授权、事务回滚、并发增量合并、长进展不阻塞阶段交接 |
| 4 运行内压缩 | `inputbudget`、`context_budget.go` | Generate/Stream 硬检查、已有来源引用复用、未知工具结果先持久化、工具配对与近期轮次保留、Eino 摘要有界且同前缀只尝试一次、摘要失败及无效引用处理 |
| 5 新输入交接 | `store_task_inputs.go`、`task_input_tools.go`、`store_task_input_lifecycle.go` | 原事件分页、assigned 不等于 applied、成功阶段才应用处理报告、未报告项归还、失败与恢复、迟到输入隔离、revision 重分配、取消后不自动唤醒、待处理项阻止 completed |

使用 `GOCACHE=/tmp/kagari-go-cache` 运行以下工程检查，均通过：

- `go test ./... -count=1`
- `go vet ./...`
- `go test -race ./internal/personal ./cmd/kagari`
- `go test -race ./internal/inputbudget ./internal/agent ./internal/workspace ./internal/textpage`
- `go build -o /tmp/kagari-context ./cmd/kagari`
- `git diff --check` 与 Go 格式检查。

真实端点用例为 `TestLivePersonalTaskInputPagingAndAcknowledgment`，使用隔离临时 SQLite、空 Profile 和虚构材料，不连接真实渠道。测试显式设置窗口 262,144、输出额度 2,500，得到输入硬上限 246,536；这是本次测试配置，不是对任意端点容量的推断。模型读取 9,000 码点共 3 页，准确报告前、中、后 3 个随机校验码，提交进展和输入处理报告；FinishStage 后验证进展、applied 状态及显式记忆来源清单均已持久保存。阶段耗时 18.373 秒。

| 模型调用 | UTF-8 字节估算 | 提供方 input tokens | 提供方 output tokens |
| --- | ---: | ---: | ---: |
| 1 | 17,584 | 5,191 | 79 |
| 2 | 31,937 | 9,533 | 143 |
| 3 | 46,406 | 13,941 | 145 |
| 4 | 51,861 | 15,345 | 269 |
| 5 | 54,481 | 15,646 | 130 |

该样本未触发摘要（0 次）；摘要成功、失败、无效引用及超限行为由模拟模型测试覆盖，未做真实端点压缩前后性能比较。验收期间真实模型曾提交非法进展状态；现已在工具 schema 声明枚举，并在 submit_progress 阶段给出校验反馈，FinishStage 复用同一规则。测试同时覆盖非法状态修正后成功保存。

运行真实用例时设置 `KAGARI_LIVE_EVAL=1`、`KAGARI_EVAL_CONFIG_DIR`，执行 `go test ./internal/personal -run '^TestLivePersonalTaskInputPagingAndAcknowledgment$' -count=1 -v`。该测试在进程内设置上述窗口容量，不改用户配置。未执行旧库迁移、真实渠道发送或长期运行压测。
