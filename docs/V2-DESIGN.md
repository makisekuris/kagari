# Kagari V2 架构与运行说明

状态：首版三个迭代已实现，工程检查与真实模型小样本分项验收通过（2026-10-30）。Telegram 收发使用模拟接口验证，尚未连接真实 bot 联调。

术语见 [CONTEXT.md](../CONTEXT.md)，取舍依据见 [V2-TRADEOFFS.md](V2-TRADEOFFS.md)。V2 面向一个使用者、一个服务实例和 Telegram 私聊，不迁移 V1 阅读归档任务。

## 1. 服务入口

`kagari run` 启动 V2 personal 服务：Telegram 收件、主对话、后台 Task、记忆维护与消息投递在同一进程中运行。进程持有数据库锁；不启动 V1 阅读任务队列或 weekly 定时调度。

`run` 要求 `telegram.allowed_user_ids` 恰好包含一个正数，`telegram.target_chat_ids` 必须为空。服务只接收该用户的私聊，并把回复发回该用户。所有回复按普通文本发送，超过 Telegram 单条长度时按最多 3500 个 UTF-16 单元分段。

| Worker | 默认并发 | 用途 |
| --- | ---: | --- |
| 主对话 | 1 | 处理用户消息和任务回调；串行组织对话回复 |
| Task 执行器 | `personal.task_concurrency`，默认 4 | 执行互相独立的读取、研究与内容生成任务；同一 Task 同时只有一个有效 Run |
| 记忆维护 | 1 | 串行处理用户输入后的自动记忆检查 |
| 消息投递 | 1 | 按顺序发送已保存的回复并记录 Telegram 回执 |

模型只在处理消息、Task 或记忆操作时调用。待处理工作保存在 SQLite；Worker 每 200ms 检查队列，空闲时不调用模型。

## 2. Task、Run 与预算

**Task** 保存持续目标、约束、版本、阶段摘要和累计执行预算。**Run** 保存一次 Task 执行或自动记忆 review 的输入、结果、错误和耗时；只有 Task Run 有关联 Task。Task 阶段结果只有摘要和来源，Store 根据执行错误与预算决定状态：成功进入 `awaiting_review`，失败进入 `failed`，预算耗尽时优先进入 `waiting_budget`。新 Run 沿用原目标和已保存成果，不把上次模型调用的位置当成可恢复检查点。

Task 状态包括 `queued`、`running`、`awaiting_review`、`waiting_input`、`waiting_budget`、`completed`、`failed` 和 `cancelled`。Run 状态包括 `queued`、`running`、`completed`、`failed` 和 `cancelled`。等待使用者的 Task 不占执行槽。

`personal.task_budget` 默认是 `30m`，按每个 Task 累计。Task Run 和对应任务回调的执行时间计入预算；排队及等待使用者不计。重启和自动继续不会重置预算。`ClaimRun` 领取 Task Run 时根据 Task 的剩余预算计算 `RemainingMS`，该值只供运行时使用，不写入数据库或 JSON。预算耗尽后保存已有阶段成果并等待使用者；用户明确授权续用时，可增加一个配置额度。`personal.task_concurrency` 默认是 `4`，必须为正数。

主 Agent 的 Task 工具包括 `create_task`、`list_tasks`、`get_task`、`update_task`、`cancel_task`、`continue_task` 和 `settle_task`；Run 工具包括 `get_run` 与 `list_runs`。Task 执行使用只读 `read_url`，启用浏览器时也可使用 `browse_url`。改目标会创建新版本，旧版本的迟到结果不能覆盖新目标。后台回调只能继续或结束自己的 Task，不能创建其他目标，也不能延长预算。长任务只排队执行，不声称在当前对话回合内已完成。

直接回复某条已送达的 Task 消息时，系统用 Telegram 回执关联该 Task；未明确回复时，主 Agent 根据上下文判断目标，存在歧义就询问。

## 3. 自动记忆与遗忘

每条直接用户消息处理完成后，记忆维护器会异步检查本条消息中的稳定事实和偏好。它只使用当前用户消息作为依据；转发材料、网页、Task 结果和 Agent 推断不会成为个人事实。明确纠正可更新已有事实；对含糊变化，不覆盖旧记忆，将待澄清问题留在 `memory_review` Run 结果中。自动检查在成功、失败和需要澄清时都保持静默，不唤醒主对话或主动通知；结果、错误和记忆变更历史仍保留，用户主动查询时再反馈。

记忆工具为 `memory_search`、`memory_get`、`memory_put`、`memory_forget` 和 `memory_history`。主 Agent 可搜索或读取当前记忆；用户明确要求保存、更正或遗忘时，`memory_put` 和 `memory_forget` 在当前工具调用中同步提交当前值、变更历史和幂等请求记录，不创建 Run。写入和遗忘都使用当前消息中的原文及预期版本；同一事件可用最新版本连续更改同一键。同一事件和 request key 的完全相同请求重放时返回原成功结果。冲突或校验失败会记入日志并返回工具错误，不写请求记录；记忆提交不随之后的对话回复失败而回滚。

遗忘会清空当前记忆值并写入 tombstone，阻止旧消息或迟到操作复活它。原有聊天、Task 历史和记忆变更历史仍保留；`memory-history` 可查询此前保存的值。因此遗忘表示不再主动使用，不是彻底擦除数据库。

## 4. 持久化与恢复

personal 服务使用 `storage.path` 指向的 SQLite 文件，创建 10 张 personal 表：

| 表 | 保存内容 |
| --- | --- |
| `personal_state` | Telegram update offset |
| `personal_meta` | owner 绑定 |
| `personal_events` | 用户、材料和 Task 回调事件；记录事件关联的 Task/Run |
| `personal_history` | 对话历史；Task 归属通过 `event_id` 关联 `personal_events` 读取 |
| `personal_tasks` | Task 目标、状态、阶段摘要和累计预算 |
| `personal_runs` | Task 执行或自动记忆 review 的输入、结果、错误和耗时 |
| `personal_requests` | 工具请求幂等记录；Task 操作关联 `task_id` 和 `run_id`，记忆操作关联 `memory_history_id` |
| `personal_memories` | 当前有效记忆与遗忘 tombstone |
| `personal_memory_history` | 版本化记忆变更；显式记忆写入的 `run_id` 为空，自动 review 写入则关联 review Run |
| `personal_outbox` | 待发送消息和回执；Task 归属通过 `event_id` 关联 `personal_events` 读取 |

显式记忆变更、其历史和请求记录在同一事务内提交；自动 review 单独保存为 Run。接收消息和推进 offset 在同一事务中完成；重复 update 不会重复入队。数据库文件权限设为仅当前用户读写。

服务启动恢复时，后台 Run 重新排队，已保存的阶段成果和累计预算保留；已耗尽预算的 Task 转为 `waiting_budget` 并通知。尚未写入的后台阶段可能重做，不恢复模型内部位置。完整来源正文留在 Run，下一阶段只携带有界摘要、来源地址和原 Run 编号，避免长材料阻止续跑。

中断的主对话回合不重新调用模型执行工具，避免模型换一种措辞后重复建任务。已经提交的操作保留；用户消息得到中断说明，未提交的部分需要重新提出。任务回调若已安排续跑，就释放原有后续 Run；若还没决定下一步，则保留阶段成果并等待用户继续。数据库绑定的 owner 与配置不一致时，服务拒绝启动。

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
  task_concurrency: 4
  task_budget: 30m
```

启动服务并直接在 bot 私聊中请求工作：

```sh
make run CONFIG=config.yaml
```

例如，发送“调查 https://example.com 上这项方案的主要取舍”；之后可问“任务进展如何？”，或说“取消任务 #1”“继续任务 #1，补查它的兼容性”。预算耗尽后，继续执行需要明确授权。

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

- 单 owner、单实例和 Telegram 私聊；不提供群聊、多租户或 fanout。
- Task 工具限于读取、研究和生成内容；不提供任意文件写入、命令执行或外部服务写入。
- 阶段恢复保留已提交结果；中断中的当前阶段可能重做，不恢复精确模型内部位置。
- 遗忘不清除聊天和 Task 历史，也不擦除记忆历史中的旧值。
- 真实模型小样本验证了当前配置下的关键行为，不代表所有输入都能正确识别事实、关联任务或判断完成。尚未做真实 Telegram 联调。

## 7. 实现顺序与验收

| 迭代 | 动机 | 已通过的验证 |
| --- | --- | --- |
| 主对话与任务队列 | 让长任务与聊天分开推进 | 四个执行器被占用时，第五个排队；聊天和记忆仍响应；取消后的迟到结果不发布 |
| 阶段推进、预算与恢复 | 长任务可以继续，重启不丢失已保存成果或刷新预算 | 同 Task 跨 Run 续跑；Run 与任务回调累计计时；耗尽预算进入等待；中断主回合不重复执行工具；长来源正文不阻断续跑 |
| 自动长期记忆 | 保存稳定背景并处理纠正与遗忘 | 真实模型识别稳定偏好、忽略临时变化、复用原键纠正、执行遗忘、跨重启调用工具召回；旧 review 和旧聊天不复活被遗忘的值 |

已运行 `make check`（全仓测试与 `go vet`）、`go test -race ./internal/personal ./internal/telegram`、CLI 构建和临时数据库的查询命令。真实模型另分项验证了任务继续、任务完成判定，以及 `read_url` 返回的随机证据实际进入阶段结果与 SQLite。所有模型样本使用虚构个人信息、空 profile、临时数据库和受控页面；没有向真实 Telegram 发消息。

真实评估发现当前模型端点同时收到工具和严格输出 Schema 时，会只回复文字而不调用工具。因此主对话与阅读阶段不再附加该输出约束，最终内容仍由程序校验。主对话回复格式不合格时，额外进行一次无工具的严格格式整理，保留操作状态、不重复执行操作。纯记忆提取继续使用严格 Schema。

重跑真实模型评估会调用本地配置中的模型服务：

```sh
KAGARI_LIVE_EVAL=1 KAGARI_EVAL_CONFIG_DIR="$PWD" \
  go test ./internal/personal -run '^TestLivePersonal' -count=1 -v
```

后续实际使用从 `make run` 和 Telegram 私聊开始；先确认唯一 owner 配置正确。
