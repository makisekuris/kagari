# Kagari V2 架构与运行说明

状态：首版三个迭代已实现，工程检查与真实模型小样本分项验收通过（2026-10-30）。2026-11-04 新增统一渠道与附件：Telegram、微信收发使用模拟接口验证，尚未连接真实 bot 联调。

术语见 [CONTEXT.md](../CONTEXT.md)，取舍依据见 [V2-TRADEOFFS.md](V2-TRADEOFFS.md)。V2 面向一个使用者、一个服务实例，每次启用 Telegram 或微信中的一个私聊渠道，不迁移 V1 阅读归档任务。

## 1. 服务入口

`kagari run` 启动 V2 personal 服务：当前渠道收件、主对话、后台 Task、记忆维护与消息投递在同一进程中运行。进程持有数据库锁；不启动 V1 阅读任务队列或 weekly 定时调度。

`personal.channel` 选择 `telegram`（默认）或 `weixin`。Telegram 要求 `telegram.allowed_user_ids` 恰好包含一个正数、`telegram.target_chat_ids` 为空；微信使用 `weixin login` 扫码绑定。服务只接收绑定使用者的私聊。文字回复按 UTF-8 字符边界切成每段最多 2,000 字节，普通附件单文件收发上限为 20,000,000 字节。登录、切换渠道和文件操作见 [渠道设计](CHANNEL-DESIGN.md)。

| Worker | 默认并发 | 用途 |
| --- | ---: | --- |
| 主对话 | 1 | 处理用户消息和任务回调；串行组织对话回复 |
| Task 执行器 | `personal.task_concurrency`，默认 4 | 执行互相独立的读取、研究与内容生成任务；同一 Task 同时只有一个有效 Run |
| 记忆维护 | 1 | 串行处理用户输入后的自动记忆检查 |
| 消息投递 | 1 | 按顺序发送已保存的回复并记录当前渠道的投递目标和回执 |

模型只在处理消息、Task 或记忆操作时调用。待处理工作保存在 SQLite；Worker 每 200ms 检查队列，空闲时不调用模型。

## 2. Task、Run 与预算

**Task** 保存持续目标、约束、版本、阶段摘要、累计执行时长和时长上限。这里的时间预算指一个 Task 各阶段及其回调合计可用的执行时间。**Run** 保存一次 Task 执行或自动记忆 review 的输入、结果、错误和耗时；只有 Task Run 关联 Task。

Task 的阶段结果包含摘要、来源、本次成功写入或修改的文件相对路径（`files`）和生成的不可变附件（`attachments`）。Store 在保存阶段结果时更新 Task 状态：成功进入 `awaiting_review`，失败进入 `failed`；如果累计执行时间已达到上限，则优先进入 `waiting_budget`。下一阶段沿用原目标和已保存成果，不恢复上次模型调用的执行位置。

Task 执行器返回文字摘要，程序记录成功写入或修改的文件路径。V1 阅读接口仍使用 `kind/body` JSON 输出。执行器将阶段摘要限制在 12,000 个字符以内，超出时在末尾加省略号；字符按 Unicode 码点计数，文件正文不受这项限制影响。模型调用报错、返回拒绝或未完成响应、摘要为空时，阶段均记为失败，已经保存的文件仍会保留。

Task 状态包括 `queued`、`running`、`awaiting_review`、`waiting_input`、`waiting_budget`、`completed`、`failed` 和 `cancelled`。Run 状态包括 `queued`、`running`、`completed`、`failed` 和 `cancelled`。等待使用者的 Task 不占执行槽。

`personal.task_budget` 设置每个 Task 的累计执行时长上限，默认为 `30m`。Task Run 和对应任务回调的执行时间都计入已用时长，排队及等待使用者的时间不计入。重启和自动继续不会清零已用时长。`ClaimRun` 领取 Task Run 时，将剩余执行时间写入 `RemainingMS`；该值只供运行时使用，不写入数据库或 JSON。

累计执行时间达到上限后，系统保存已有阶段成果并等待使用者。用户明确授权延长执行时间后，系统提高时长上限，每次增加的时长等于 `personal.task_budget` 的配置值。`personal.task_concurrency` 默认是 `4`，必须为正数。

主 Agent 通过 `create_task`、`list_tasks`、`get_task`、`update_task`、`cancel_task`、`continue_task` 和 `settle_task` 管理 Task，通过 `get_run` 与 `list_runs` 查看 Run。Task 执行器可用 `read_url` 读取网页，启用浏览器后还可用 `browse_url`。`ls`、`read_file`、`write_file` 和 `edit_file` 只操作当前 Task 的工作区。

修改目标会创建新版本，旧版本迟到的结果不能覆盖新目标。后台回调只能继续或结束自己的 Task，不能创建其他目标，也不能提高累计执行时长上限。创建长任务的对话回合只确认任务已入队，不能把入队说成完成。

直接回复某条已送达的 Task 消息时，系统用来源渠道、账号、会话和平台消息 ID 关联该 Task；未明确回复时，主 Agent 根据上下文判断目标，存在歧义就询问。

## 3. 自动记忆与遗忘

每条直接用户消息处理完成后，记忆维护器会异步检查本条消息中的稳定事实和偏好。它只使用当前用户消息作为依据；转发材料、网页、Task 结果和 Agent 推断不会成为个人事实。明确纠正可更新已有事实；对含糊变化，不覆盖旧记忆，将待澄清问题留在 `memory_review` Run 结果中。自动检查在成功、失败和需要澄清时都保持静默，不唤醒主对话或主动通知；结果、错误和记忆变更历史仍保留，用户主动查询时再反馈。

记忆工具为 `memory_search`、`memory_get`、`memory_put`、`memory_forget` 和 `memory_history`。主 Agent 可搜索或读取当前记忆；用户明确要求保存、更正或遗忘时，`memory_put` 和 `memory_forget` 在当前工具调用中同步提交当前值、变更历史和幂等请求记录，不创建 Run。写入和遗忘都使用当前消息中的原文及预期版本；同一事件可用最新版本连续更改同一键。同一事件和 request key 的完全相同请求重放时返回原成功结果。冲突或校验失败会记入日志并返回工具错误，不写请求记录；记忆提交不随之后的对话回复失败而回滚。

遗忘会清空当前记忆值并写入 tombstone，阻止旧消息或迟到操作复活它。原有聊天、Task 历史和记忆变更历史仍保留；`memory-history` 可查询此前保存的值。因此遗忘表示不再主动使用，不是彻底擦除数据库。

## 4. 持久化与恢复

personal 服务使用 `storage.path` 指向的 SQLite 文件，创建 13 张 personal 表：

| 表 | 保存内容 |
| --- | --- |
| `personal_schema` | personal schema 版本 |
| `personal_meta` | 数据库标识 `store_id` |
| `personal_bindings` | 各渠道绑定的 Bot 账号和使用者 |
| `personal_inbox` | 按渠道及 Bot 账号保存接收游标 |
| `personal_attachments` | 附件元数据、受控文件位置及删除状态 |
| `personal_events` | 用户、材料和 Task 回调事件；记录事件关联的 Task/Run |
| `personal_history` | 对话历史；Task 归属通过 `event_id` 关联 `personal_events` 读取 |
| `personal_tasks` | Task 目标、状态、阶段摘要、累计执行时长和时长上限 |
| `personal_runs` | Task 执行或自动记忆 review 的输入、结果、错误和耗时 |
| `personal_requests` | 工具请求幂等记录；Task 操作关联 `task_id` 和 `run_id`，记忆操作关联 `memory_history_id` |
| `personal_memories` | 当前有效记忆与遗忘 tombstone |
| `personal_memory_history` | 版本化记忆变更；显式记忆写入的 `run_id` 为空，自动 review 写入则关联 review Run |
| `personal_outbox` | 待发送消息和回执；Task 归属通过 `event_id` 关联 `personal_events` 读取 |

显式记忆变更、其历史和请求记录在同一事务内提交；自动 review 单独保存为 Run。接收批次和推进渠道游标在同一事务中完成；消息按渠道、账号、发送者及消息 ID 去重。数据库文件权限设为仅当前用户读写。

服务重启时，中断的后台 Run 重新排队，已保存的阶段成果和已用执行时间都保留。累计执行时间已达到上限的 Task 转为 `waiting_budget` 并通知使用者。中断的阶段可能重新执行，模型内部的执行位置不会恢复。完整来源正文保存在 Run 中；下一阶段接收限制长度后的摘要、来源地址、文件相对路径、独立附件引用和原 Run 编号，避免一次传入过长的材料。

每个 Task 使用固定的本地工作区：`<storage.path>.workspaces/<store_id>/tasks/<task_id>/`。数据库元数据中的 `store_id` 用来区分不同数据库，避免新数据库中的同号 Task 读写旧文件。同一 Task 继续执行、修改目标或在服务重启后恢复时，执行器都使用原目录。文件工具只接受相对路径，并通过 `os.Root` 阻止路径和符号链接访问工作区外的文件。文件必须是 UTF-8 文本，每个文件最多 1 MiB。

`write_file` 创建或完整覆盖文件，覆盖前应先读取。`edit_file` 要求待替换文本在文件中恰好出现一次；找不到或出现多次都会报错。写入时，工具先保存同目录下的临时文件，再原子替换目标文件。写入一旦成功，即使阶段随后失败或取消，也不会撤销修改。

每次文件操作前，工具都会检查 Run 是否仍有效。同一 Task 的旧阶段尚未退出时，新阶段会等待，等待时间不计入执行时长。等待期间可取消新阶段，其他 Task 仍可执行。执行器的提示词要求它在继续任务或重启恢复后，先读取现有文件，再决定如何修改。

Run 的 `files` 字段列出本次成功写入或修改的文件相对路径，路径相对于当前 Task 的工作区。这些路径指向当前文件，不保存历史版本，也不表示文件已作为附件发送。程序将文件路径随阶段结果写入现有 JSON 字段，无需修改数据库表结构。任务还可用 `read_attachment` 读取选定材料，用 `write_attachment` 保存独立的文本附件；主 Agent 只发送最终回复中选择的附件 ID。工作目录文件不会自动转为附件。备份或迁移服务时，需同时保留数据库、工作区、附件和凭证。本版不接入 OSS、命令执行或任意跨 Task 工作目录访问。

发给主 Agent 的 Task 回调结果最多包含 2,000 个字符，同样按 Unicode 码点计数。结果过长时，程序先省略来源正文和链接，再缩短摘要及来源信息，优先保留完整的文件路径和附件 ID，保证结果仍是有效 JSON。如果引用列表本身仍超长，就省略整条引用，并在摘要中说明。数据库中保存的阶段结果不受这次裁剪影响，仍可通过 `get_run` 查看。

中断的主对话回合不重新调用模型执行工具，避免模型换一种措辞后重复建任务。已经提交的操作保留；用户消息得到中断说明，未提交的部分需要重新提出。任务回调若已安排续跑，就释放原有后续 Run；若还没决定下一步，则保留阶段成果并等待用户继续。渠道绑定与配置或微信凭证不一致时，服务拒绝启动；更换微信使用者需先停止服务再显式执行 `weixin rebind`。

当前 V2 schema 不支持旧 V2 personal 数据库，也不迁移其中的数据。使用者需为当前版本指定新的 `storage.path`；服务不会自动删除旧数据库。V1 表仍由旧 CLI 单独读取，不迁移为 personal Task。

若进程在发送期间退出，该 outbox 项恢复为 `uncertain`，不会自动重发，以免重复发送；已确认的回执保留。可确认未送达的失败最多尝试三次，包含首次尝试。发送结果未知时需要使用者判断是否重发。

V2 不把 V1 的 `jobs`、归档、周报快照或 Telegram offset 导入 personal Task。旧表可留在同一 SQLite 文件中，旧 CLI 仍读取旧表；V1 `status` 等编号不能代替 V2 `tasks`/`runs` 编号。

## 5. 启动与使用

在 `.env` 设置模型 endpoint、模型名、API key 和 Telegram bot token；在 `config.yaml` 配置唯一 owner 与 personal 默认值：

```yaml
storage:
  path: data/kagari.db

telegram:
  allowed_user_ids: [123456789]
  target_chat_ids: []

personal:
  channel: telegram
  task_concurrency: 4
  task_budget: 30m
```

启动服务并直接在 bot 私聊中请求工作：

```sh
make run CONFIG=config.yaml
```

例如，发送“调查 https://example.com 上这项方案的主要取舍”；之后可问“任务进展如何？”，或说“取消任务 #1”“继续任务 #1，补查它的兼容性”。累计执行时间达到上限后，需要用户明确授权增加执行时间才能继续。

稳定偏好会在用户消息后自动检查，例如“以后回答请简洁一些，但保留关键依据”。也可以直接纠正或遗忘：“更正：我现在主要使用 Rust”“忘记我偏好简洁回答这件事”。这些操作进入后台队列，不应在操作完成前宣称已保存或已遗忘。

以下本地检查命令只读取或导出 V2 personal 表，输出 JSON，不恢复或改写运行中服务的状态：

```sh
./bin/kagari -config config.yaml tasks
./bin/kagari -config config.yaml tasks 1
./bin/kagari -config config.yaml runs 1
./bin/kagari -config config.yaml memories 'answer style'
./bin/kagari -config config.yaml memory-history answer_style
./bin/kagari -config config.yaml events --limit 20
./bin/kagari -config config.yaml trace --event 42
./bin/kagari -config config.yaml trace --task 1 --full
```

`tasks` 可列出最近任务或查看一个 Task；`runs` 列出该 Task 的执行；`memories` 按键或值搜索当前生效的记忆；`memory-history` 查看指定键的变更，省略键时查看最近变更。旧的 `read`、`analyze`、`digest`、`status`、`export`、`retry`、`retry-delivery` 和 `db` CLI 仍用于 V1 阅读归档工作，不属于 V2 personal Task 管理入口。

`events` 列出近期事件及状态；`trace` 按事件或 Task 关联主对话、模型、工具、后台执行、记忆维护和投递日志，并附当前数据库快照。默认只显示元数据，`--full` 显示脱敏后的正文。V2 服务自动保存日志并输出终端摘要，配置与失败语义见[观测性说明](V2-OBSERVABILITY.md)。

## 6. 首版边界

- 单 owner、单实例，每次启用一个 Telegram 或微信私聊渠道；不提供群聊、多租户或 fanout。
- Task 工具支持读取、研究、生成内容和任务工作区内的文本文件读写；另外支持受控附件读写；不提供任意本地路径写入、命令执行或外部服务写入。
- 阶段恢复保留已提交结果；中断中的当前阶段可能重做，不恢复精确模型内部位置。
- 遗忘不清除聊天和 Task 历史，也不擦除记忆历史中的旧值。
- 真实模型小样本验证了当前配置下的关键行为，不代表所有输入都能正确识别事实、关联任务或判断完成。尚未做真实 Telegram 或微信联调。

## 7. 实现顺序与验收

| 迭代 | 动机 | 已通过的验证 |
| --- | --- | --- |
| 主对话与任务队列 | 让长任务与聊天分开推进 | 四个执行器被占用时，第五个排队；聊天和记忆仍响应；取消后的迟到结果不发布 |
| 阶段推进、计时与恢复 | 长任务可分阶段执行，重启后保留已保存成果和已用执行时间 | 同 Task 跨 Run 继续；Run 与任务回调累计计时；达到时长上限后等待用户授权；中断主回合不重复执行工具；长来源正文不阻止下一阶段执行 |
| 自动长期记忆 | 保存稳定背景并处理纠正与遗忘 | 真实模型识别稳定偏好、忽略临时变化、复用原键纠正、执行遗忘、跨重启调用工具召回；旧 review 和旧聊天不复活被遗忘的值 |

已运行 `make check`（全仓测试与 `go vet`）、`go test -race ./internal/personal ./internal/telegram`、CLI 构建和临时数据库的查询命令。真实模型另分项验证了任务继续、任务完成判定，以及 `read_url` 返回的随机证据实际进入阶段结果与 SQLite。所有模型样本使用虚构个人信息、空 profile、临时数据库和受控页面；没有向真实 Telegram 发消息。

工作区测试覆盖了生成 `report.md`、继续任务时只修改第三段、重启恢复后再次修改同一文件三个阶段，并逐字节检查其他内容是否保持不变。回归测试使用预设模型响应、实际文件工具和 SQLite；`TestLivePersonalTaskWorkspace` 则由真实模型调用文件工具完成这三个阶段，两项测试均已通过。两项测试都通过关闭并重开数据库、重建执行器来验证恢复，没有执行进程强杀测试。

三个问题的回归测试也已通过：超长摘要不再阻止阶段结果保存；等待同一 Task 的旧阶段退出时不累计执行时间；长来源正文不会挤掉回调结果中的文件路径。相关检查包括 `make check`、`go test -race ./internal/personal ./internal/workspace` 和 CLI 构建。

可单独运行报告回归测试：`go test ./internal/personal -run '^TestTaskWorkspaceReportContinuesAfterRestart$' -count=1 -v`。真实模型测试使用下方的 opt-in 环境变量；只验证工作区时，将 `-run` 设为 `'^TestLivePersonalTaskWorkspace$'`。

真实评估发现当前模型端点同时收到工具和严格输出 Schema 时，会只回复文字而不调用工具。因此主对话与阅读阶段不再附加该输出约束，最终内容仍由程序校验。主对话回复格式不合格时，额外进行一次无工具的严格格式整理，保留操作状态、不重复执行操作。纯记忆提取继续使用严格 Schema。

重跑真实模型评估会调用本地配置中的模型服务：

```sh
KAGARI_LIVE_EVAL=1 KAGARI_EVAL_CONFIG_DIR="$PWD" \
  go test ./internal/personal -run '^TestLivePersonal' -count=1 -v
```

后续实际使用从 `make run` 和 Telegram 私聊开始；先确认唯一 owner 配置正确。
