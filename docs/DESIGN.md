# 设计说明

[返回 README](../README.md)

## 系统边界

Kagari 是 Go 单进程应用，使用本地 SQLite 保存提交、任务、分析、来源缓存、周报和各目标的投递状态。读取、模型分析、分发、队列和 Telegram 适配器都在一个进程内协作；CLI 和 Telegram 共用任务处理与存储路径。

项目使用 Eino typed agent 和 OpenAI Responses 兼容模型、标准库 HTTP、Readability、Telegram long polling、Viper、Zap 与 `database/sql`/`modernc.org/sqlite`。网页读取覆盖普通 HTTP 页面与公开 X 单帖 oEmbed。当前不支持任意搜索、登录或验证码绕过、付费墙绕过、全文镜像，也不会在网页读取失败后改用浏览器。

## 模块职责

| 模块 | 职责 |
| --- | --- |
| `cmd/kagari` | CLI 命令分发与进程初始化 |
| `internal/config` | 配置类型、默认值、环境覆盖与启动校验 |
| `internal/reader` | URL 规范化、安全拨号、网页提取和 X oEmbed |
| `internal/agent` | 单条分析、周报 Agent、工具调用、提示词与结构化结果校验 |
| `internal/digest` | 整理周报输入、按 `CacheKey` 去重、检查条目分组与引用、渲染和旧报告兼容 |
| `internal/store` | SQLite schema、任务队列、归档、缓存、投递 outbox 与维护查询 |
| `internal/app` | Worker、命令权限、任务重试、投递与周报调度 |
| `internal/distribution` | 根据渠道和地址生成投递计划、去重目标、调用渠道发送实现及统一发送错误契约 |
| `internal/telegram` | Bot 客户端、long polling、update offset、消息解析和 Telegram 分发适配器 |
| `internal/render` | 单条分析报告与 Telegram 消息分段 |
| `internal/domain` | 提交、来源、分析、任务、投递和周报领域类型 |

## 单条分析数据流

1. CLI 或 Telegram 将输入转换为 `Submission`。转发讨论文本作为独立来源保存，提交者的身份和权限仍由 Telegram 用户 ID 确定。
2. Agent 规范化 URL，并以用户、输入、profile、提示词、模型和分析设置计算分析缓存键；收录时间不参与键的计算。
3. Reader 读取已提交的链接。Agent 只能通过 `read_source` 追读已发现的链接；会话限制来源数、补读数和深度。
4. 模型返回严格结构化结果。程序校验必填栏目、分类和来源引用后，才将结果作为成功分析发布。
5. Worker 将渲染后的文章交给分发层，按入队时保存的渠道和地址生成投递计划。文章只生成一次，各渠道适配器负责分段与发送；结果、分段 outbox 和来源缓存原子写入 SQLite。
6. 独立 delivery 状态机调用渠道适配器发送。顺序和唯一性按任务、渠道、地址、分段编号确定，一个目标失败不会阻塞其他目标。结果未知的发送不会自动重试，以避免静默重复消息。

当前配置接入 Telegram 多目标，确认、命令回复和处理失败通知只回私聊。旧单目标配置与旧任务继续沿用原目标；启动时迁移 outbox，保留已有状态和消息编号。CLI 分析与周报命令继续只输出本地结果。分发层不依赖 Telegram 客户端，新渠道通过注册正文处理和发送函数接入；目前未实现其他渠道或内容分类路由。

分析结果分开保存来源事实、讨论者观点和评价。失败的读取可作为来源状态保留，但不能作为成功证据；每条结论的来源 ID 必须指向实际读取成功的来源。模型生成的栏目名称是展示文案，不改变证据含义。旧归档没有栏目字段时使用兼容默认值。

分析缓存键包含基础提示词、人格与输出规则和分析版本。profile 或这些提示词变化会生成不同的缓存键；流式传输方式不参与键的计算。

通用角色说明在 `internal/agent/prompt/main.go`，分析与周报输出规则分别在 `analysis.go` 和 `digest.go`；人格内容在 `persona.go`。`internal/agent/prompt/system_prompt/base.md` 和 `character.md` 是历史参考，程序未读取这两份文件。修改实际使用的 Go 提示词后需要重新构建；运行中的服务在启动时读取 `profile.md`，修改后需重启。周报重试继续使用首次处理时保存的指令与偏好。

## 周报数据流

周报先按用户和收录时间选择已完成分析，再对非空且相同的 `CacheKey` 只保留一条；没有缓存键的条目分别保留。Agent 只可合并内容重复的分析，仅主题相近的条目分别保留。每条输入必须且只能进入一个回顾项，引用必须来自该项对应的可用来源。程序保留并附加已知的来源缺失、截断和不确定性；校验能检查结构和引用归属，不能证明摘要与原文语义一致，也不能判断合并的条目是否确实重复。

周报采用固定截止时间和首次处理时的材料快照。输入超限或模型输出无效会使本次尝试失败并保留快照，重试沿用同一输入。校验后的周报正文先持久化，再与完成状态及投递 outbox 一起提交；进程恢复时可回放已保存正文。相同版本、用户和精确时间范围只创建一个任务。不同版本分别创建任务，历史结果仍可读取。

调度器从启用后的持久化游标补跑到期周期，不回填启用前的周期。每个历史周期使用各自的计划触发时间作为截止时间，而不是使用补跑时的当前时间。

## 可靠性与安全契约

- Telegram update 入队后才推进持久化 offset；入队与 offset 更新在同一事务中完成。
- 任务结果/outbox 原子保存，投递状态与任务状态分开管理。未知投递状态需要人工选择是否重发。
- 任务失败时保留已取得的部分结果。处理上下文被取消时，任务重新排队，本次不计入尝试次数。普通失败按退避时间重试；尝试次数达到 `max_attempts` 后任务标记为失败，分析与周报任务在有通知目标时发送最终失败通知。上限包含首次尝试，手动重试会将尝试计数重置为 0。
- Reader 对每个 DNS 结果和重定向目标重新执行公网地址校验，并直接连接已校验 IP，防止 DNS rebinding。只有显式配置的 CIDR 可放行指定非公网地址；混合公网和未放行地址的 DNS 结果仍会被拒绝。
- 网页、转发文本和工具返回都视为不可信内容，不能改变模型权限或触发允许列表外的读取。
- 本地归档包含提交内容与网页提取正文；日志也可能包含模型输出和部分阅读内容，需按私人资料保管。

## 浏览器读取限制

配置结构预留 `reader.browser_fallback` 的引擎、启动模式和远程 endpoint 字段，当前没有浏览器读取实现。`enabled: true` 会在启动校验时被拒绝。没有引入 Playwright 绑定、driver、浏览器二进制或其系统依赖。

## 验证边界

单元测试和本地模拟接口覆盖配置、URL 安全策略、Responses 工具往返、结构化结果、SQLite 状态转换、周报校验和消息投递状态。本地测试不能验证任意模型 endpoint、X 页面或 Telegram bot 的真实兼容性；外部联调需要部署者提供 endpoint、凭证、allowlist 和发送权限。

## 参考资料

- [Eino AgenticModel 指南](https://www.cloudwego.io/docs/eino/core_modules/components/agentic_chat_model_guide/)
- [Eino Responses 适配器](https://github.com/cloudwego/eino-ext/tree/main/components/model/agenticopenai)
- [go-telegram/bot](https://github.com/go-telegram/bot) 与 [Telegram getUpdates](https://core.telegram.org/bots/api#getupdates)
- [go-readability](https://codeberg.org/readeck/go-readability/src/branch/v2)
- [modernc SQLite](https://pkg.go.dev/modernc.org/sqlite)
