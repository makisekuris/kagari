# Kagari

Kagari 是一个 Go 个人 Agent。`kagari run` 启动面向单一使用者的 Telegram 私聊服务，持续推进后台任务并维护长期记忆。`read`、`analyze`、`digest`、`status`、`export`、`retry`、`retry-delivery` 和 `db` CLI 仍操作 V1 阅读归档；V2 服务不处理 V1 队列或调度 V1 周报。

Kagari is a personal agent written in Go. `kagari run` starts a Telegram private-chat service for one owner, continuing background tasks and maintaining long-term memory. The `read`, `analyze`, `digest`, `status`, `export`, `retry`, `retry-delivery`, and `db` CLI commands still operate on V1 reading archives; the V2 service does not process the V1 queue or schedule V1 digests.

运行需要 Go 1.27；进程锁使用 `flock`，支持 macOS 和 Linux。部署者提供模型 endpoint 和 Telegram bot；V2 只向配置的 owner 私聊回复。

Requires Go 1.27. The process lock uses `flock` and supports macOS and Linux. You provide the model endpoint and Telegram bot; V2 replies only to the configured owner's private chat.

## 日常操作 / Everyday commands

在项目根目录运行 `make` 或 `make help` 查看命令。需要 Go 和 Make；macOS 和 Linux 的 Make 均可使用。

Run `make` or `make help` from the project root to list commands. Requires Go and Make; Make on macOS and Linux is supported.

| 命令 / Command | 用途 / Purpose |
| --- | --- |
| `make init` | 复制示例配置，保留已有文件 / Copy example configuration, preserving existing files |
| `make build` | 为当前平台构建 `bin/kagari` / Build `bin/kagari` for the current platform |
| `make build-mac` | 构建 macOS arm64 产物 / Build for macOS arm64 |
| `make build-linux` | 构建 Linux amd64 产物 / Build for Linux amd64 |
| `make run` | 构建并启动 V2 Telegram personal 服务 / Build and start the V2 Telegram personal service |
| `make cli ARGS='...'` | 构建并执行任意 CLI 子命令 / Build and execute any CLI subcommand |
| `make check` | 运行全部测试和 `go vet` / Run all tests and `go vet` |
| `make test` / `make vet` | 单独运行测试或静态检查 / Run tests or static checks separately |
| `make fmt` | 格式化 Go 代码 / Format Go code |

GitHub Actions 在每次 push 和 PR 上运行 `make check`，Go 版本取自 `go.mod`。

GitHub Actions runs `make check` on pushes and pull requests, using the Go version in `go.mod`.

```sh
make cli ARGS='read https://example.com/article'
make cli ARGS='analyze -note "关注系统设计" -user 123456 https://example.com/article'
make cli ARGS='analyze -question "这项设计有哪些取舍？" -user 123456'
make cli ARGS='digest -user 123456'
make cli ARGS='status 17'
make cli ARGS='db info'
make cli ARGS='tasks'
make cli ARGS='tasks 1'
make cli ARGS='runs 1'
make cli ARGS='memories "answer style"'
make cli ARGS='memory-history answer_style'
make run CONFIG=path/to/config.yaml
```

`run` 和 `cli` 每次先构建，构建失败则停止。`CONFIG` 默认是 `config.yaml`，可在这两个目标上覆盖；`.env` 仍从当前工作目录读取。`ARGS` 按 shell 参数解析，包含空格的单个参数需像上面的 `-note` 一样加引号。V2 检查命令输出 JSON；旧版 `status`、`export` 和 retry 命令读取 V1 归档队列。数据库命令的范围见下文。

`run` and `cli` build first on every invocation and stop if the build fails. `CONFIG` defaults to `config.yaml` and can be overridden for either target; `.env` is still loaded from the current working directory. `ARGS` is parsed as shell arguments; quote individual arguments containing spaces as shown with `-note`. The V2 inspection commands print JSON; `status`, `export`, and retry commands inspect the separate V1 archive queue. See below for database limits.

跨平台构建关闭 CGO，产物名称包含平台和架构。macOS 默认使用 Apple Silicon 的 `arm64`，Intel Mac 使用 `MAC_ARCH=amd64`；Linux 默认使用 `amd64`，ARM 服务器使用 `LINUX_ARCH=arm64`：

Cross-platform builds disable CGO and include the platform and architecture in the output name. macOS defaults to `arm64` for Apple Silicon; use `MAC_ARCH=amd64` for Intel Macs. Linux defaults to `amd64`; use `LINUX_ARCH=arm64` for ARM servers:

```sh
make build-mac                    # bin/kagari-darwin-arm64
make build-mac MAC_ARCH=amd64     # bin/kagari-darwin-amd64
make build-linux                  # bin/kagari-linux-amd64
make build-linux LINUX_ARCH=arm64 # bin/kagari-linux-arm64
```

将对应产物与本地配置放到目标机器，直接运行该文件；`make run` 和 `make cli` 使用本机的 `bin/kagari`。

Place the matching binary and local configuration on the target machine and run that binary directly. `make run` and `make cli` use the native `bin/kagari`.

## Quickstart / 快速开始

### 1. 配置 / Configure

复制示例文件，保留已有的本地配置：

Copy the examples, leaving existing local files unchanged:

```sh
make init
```

在 `.env` 中填写 `KAGARI_MODEL_BASE_URL`、`KAGARI_MODEL_NAME`、`KAGARI_MODEL_API_KEY` 和 `KAGARI_TELEGRAM_TOKEN`。在 `config.yaml` 中填入唯一一个正数 `telegram.allowed_user_ids`，并保持 `target_chat_ids` 为空；`run` 不支持多个 owner 或群聊目标。

Set `KAGARI_MODEL_BASE_URL`, `KAGARI_MODEL_NAME`, `KAGARI_MODEL_API_KEY`, and `KAGARI_TELEGRAM_TOKEN` in `.env`. In `config.yaml`, set exactly one positive `telegram.allowed_user_ids` entry and keep `target_chat_ids` empty; `run` does not support multiple owners or group destinations.

```yaml
telegram:
  allowed_user_ids: [123456789]
  target_chat_ids: []

personal:
  task_concurrency: 4
  task_budget: 30m
```

### 2. 构建并试读 / Build and read

```sh
make cli ARGS='read https://example.com/article'
```

`read` 不需要模型凭证。配置好模型后，可分析链接：

`read` does not require model credentials. Configure a model endpoint to analyze a link:

```sh
make cli ARGS='analyze -user 123456 https://example.com/article'
```

启动 V2 Telegram 服务后，在 owner 私聊中直接发送任务或偏好：

Start the V2 Telegram service, then send a task or preference in the owner's private chat:

```sh
make run
```

```text
调查 https://example.com 的方案和主要风险。
以后回答请简洁一些，但保留关键依据。
```

## 配置 / Configuration

所有命令都会读取**当前工作目录**的 `.env`，无需手动 `source`。配置优先级为 **系统环境变量 > .env > YAML > 默认值**；显式设置的空值也会覆盖低优先级配置。`-config` 只指定 YAML 路径，不改变 `.env` 查找目录。

Commands load `.env` from the current working directory; no `source` command is needed. Precedence is **process environment > .env > YAML > defaults**. An explicitly set empty value overrides lower-priority values. `-config` selects the YAML file without changing where `.env` is loaded from.

| 设置 / Setting | 默认值 / Default | 说明 / Description |
| --- | --- | --- |
| `storage.path` | `data/kagari.db` | SQLite 数据库 / SQLite database |
| `profile_path` | `profile.md` | V2 Agent 的 owner 背景与表达偏好 / Owner context and style for the V2 agent |
| `max_attempts` | `6` | 包含首次尝试 / Includes the first attempt |
| `agent.streaming` | `false` | 启用 Responses SSE / Enables Responses SSE |
| `personal.task_concurrency` | `4` | 并行 Task worker 数，必须为正数 / Concurrent task workers; must be positive |
| `personal.task_budget` | `30m` | 每个 Task 的累计执行预算 / Cumulative execution budget per Task |
| `weekly.enabled` | `false` | V1 weekly 调度设置；V2 `run` 不调度周报 / V1 weekly scheduler setting; V2 `run` does not schedule digests |
| `weekly.max_input_chars` | `120000` | 周报完整模型输入字符上限 / Maximum serialized digest input size |

`.env`、`config.yaml` 和 `profile.md` 已被 Git 忽略。不要将真实凭证写入示例文件或提交到仓库。

`.env`, `config.yaml`, and `profile.md` are Git-ignored. Keep real credentials out of example files and commits.

### 人格替换 / Persona injection

V2 `run` 通过 `persona.Default()` 读取人格提示词，并在启动时载入 `profile.md`。角色选择集中在 `internal/persona/main.go`；替换实现后重新构建并重启服务，修改 `profile.md` 后重启服务。

V2 `run` reads the persona prompt through `persona.Default()` and loads `profile.md` at startup. Persona selection lives in `internal/persona/main.go`; rebuild and restart after changing the implementation, and restart after changing `profile.md`.

内置永雏塔菲（默认）和明前奶绿。使用奶绿时，将 `internal/persona/main.go` 的导入改为 `nailv "kagari/internal/persona/nailv"`，并在 `Default()` 中返回 `nailv.Nailv{}`。奶绿人格参考[公开角色资料](https://laplace.live/about)，采用花店店员的背景和亲切、慵懒、偶尔吐槽的语气。

Built-in personas are Taffy (default) and Mingqian Nailv. To use Nailv, change the import in `internal/persona/main.go` to `nailv "kagari/internal/persona/nailv"` and return `nailv.Nailv{}` from `Default()`. Nailv draws on the [public character profile](https://laplace.live/about), using a flower shop assistant background and a friendly, laid-back tone with occasional teasing.

旧版 `analyze` 和 `digest` CLI 仍使用阅读 Agent 与其输出格式；它们不由 V2 `run` 调度。

The legacy `analyze` and `digest` CLI commands still use the reading agent and its output format; V2 `run` does not schedule them.

### 旧阅读输出 / Legacy reading output

主对话和读取阶段在调用工具时不附加 JSON Schema 输出约束，最终结果由程序校验。主对话格式不合格时，只额外进行一次无工具的严格格式整理，不重做操作；自动记忆提取也使用严格 JSON Schema，因此模型端点仍需支持该能力。旧版 `analyze` 返回 `kind` 和 `body`：`analysis` 是 Markdown 分析，`chat` 是普通对话或澄清。`digest` CLI 继续直接生成归档周报。

Conversation and reading tool loops omit wire-level JSON Schema and validate the final result locally. A malformed conversation reply gets one tool-free formatting attempt without replaying actions. Automatic memory extraction also uses strict JSON Schema, so the endpoint must still support it. Legacy `analyze` returns `kind` and `body`: `analysis` is Markdown, while `chat` is a conversational reply or clarification. The `digest` CLI still generates archive digests directly.

## CLI 命令 / CLI commands

全局 `-config` 必须写在子命令之前。以下命令适用于本地构建的 `bin/kagari`：

Place the global `-config` flag before the subcommand. These examples use the locally built `bin/kagari`:

```sh
./bin/kagari -config config.yaml read https://example.com/article
./bin/kagari -config config.yaml analyze -note '关注系统设计' -user 123456 https://example.com/article
./bin/kagari -config config.yaml digest -user 123456
./bin/kagari -config config.yaml status 17
./bin/kagari -config config.yaml export 17
./bin/kagari -config config.yaml retry 17
./bin/kagari -config config.yaml retry-delivery 17
./bin/kagari -config config.yaml tasks
./bin/kagari -config config.yaml tasks 1
./bin/kagari -config config.yaml runs 1
./bin/kagari -config config.yaml memories 'answer style'
./bin/kagari -config config.yaml memory-history answer_style
./bin/kagari -config config.yaml db info
```

- V1 `read URL` 读取 URL，不调用模型；`analyze [-question TEXT] [-note TEXT] [-user ID] [URL...]` 分析或探索问题并归档结果。`digest [-user ID] [-cutoff RFC3339]` 默认回顾截止时间前七个日历日；`-start` 和 `-end` 必须同时提供，按包含开始、不包含结束处理，且不能与 `-cutoff` 混用。它是手动 CLI 操作，V2 `run` 不启动 weekly scheduler。
- V1 `status ID` 查看旧归档任务和投递状态，`export ID` 输出任务 JSON，`retry ID` 与 `retry-delivery ID` 操作旧归档队列。它们的 ID 不是 V2 Task/Run ID。
- V2 `tasks [TASK_ID]` 输出最近 Task 或单个 Task 的 JSON；`runs TASK_ID` 输出该 Task 的 Run 记录。
- V2 `memories [QUERY]` 以不区分大小写的子串搜索当前有效的记忆；省略查询时列出最近记忆。`memory-history [KEY]` 输出最近记忆变更；传入 KEY 时精确筛选该键，历史中可能包含已遗忘前的值。这些命令只读 personal 表，不恢复或修改服务状态。
- `db` 仍管理 V1 阅读归档的表；它不会清除 V2 Task、Run、记忆或 personal Telegram offset。

The legacy `read URL` command fetches one URL without a model call. `analyze [-question TEXT] [-note TEXT] [-user ID] [URL...]` analyzes links or explores a question and archives the result. `digest [-user ID] [-cutoff RFC3339]` defaults to the seven calendar days before the cutoff. Use `-start` and `-end` together for an inclusive start and exclusive end; they cannot be combined with `-cutoff`. Digest generation is a manual CLI operation, and V2 `run` does not start the weekly scheduler.

Legacy `status ID` shows archive job and delivery state, `export ID` prints job JSON, and `retry ID` / `retry-delivery ID` operate on the old archive queue. Their IDs are not V2 Task or Run IDs.

V2 `tasks [TASK_ID]` prints recent Tasks or one Task as JSON; `runs TASK_ID` prints that Task's Run records. `memories [QUERY]` performs a case-insensitive substring search over active memory keys and values; omitting QUERY lists recent memories. `memory-history [KEY]` prints recent memory changes, optionally filtered by an exact key. History can include values from before a memory was forgotten. These commands read personal tables without recovering or changing service state.

`db` still manages V1 reading-archive tables. It does not clear V2 Tasks, Runs, memories, or the personal Telegram offset.

V2 通过 Telegram owner 私聊接收任务、取消或继续任务，也可自然表达稳定偏好、纠正和遗忘请求。例如：

V2 accepts tasks and requests to cancel or continue them in the owner's Telegram private chat. Stable preferences, corrections, and forgetting can also be phrased naturally. For example:

```text
调查 https://example.com 的方案和主要风险。
这个任务进展如何？
取消任务 #1。
继续任务 #1，补查兼容性；我授权再用一个预算周期。
以后回答请简洁一些，但保留关键依据。
更正：我现在主要使用 Rust。
忘记我偏好简洁回答这件事。
```

每条直接用户消息处理后都会异步检查稳定事实和偏好。自动记忆检查始终静默，包括失败和需要澄清的情况；结果与错误保留在 Run 中，查询个人记忆可确认已生效的内容。用户明确要求保存、更正或遗忘时，主对话会同步提交记忆、变更历史和请求记录；成功后才可报告已生效，校验或版本冲突会作为工具错误返回。Task 和记忆工具只改写本地 personal 数据，不开放任意文件写入、命令执行或外部写操作；Task 可以只读网页。Telegram 回复全部使用普通文本，每条 outbox 分段最多 3500 个 UTF-16 单元；每段最多尝试三次，包含首次尝试。发送结果未知时不会自动重发，避免重复消息。

After each direct user message, a background reviewer checks for durable facts and preferences. Automatic memory review stays silent, including failures and clarification; results and errors remain in Runs. When the user explicitly asks to save, correct, or forget a memory, the conversation synchronously commits the memory, its change history, and the request record. It can report success only after the commit; validation and version conflicts return as tool errors. Task and memory tools change local personal data but do not allow arbitrary file writes, command execution, or external writes; Tasks can read web pages. Telegram replies use plain text, split into outbox parts of at most 3500 UTF-16 units. Each part gets at most three total attempts, including the first. An unknown send outcome is not retried automatically, to avoid duplicate messages.

V2 的 Task 状态、阶段成果、预算、记忆和回执保存在 `storage.path` 指向的 SQLite 文件中。Task Run 的剩余预算在领取时按 Task 的累计预算计算，不单独持久化；启动后中断的后台阶段沿用 Task 剩余预算重新排队。中断的主对话保留已经提交的操作并说明中断，未提交的要求需要重新提出，以免重复建任务。发送中断的消息标记为结果不确定；旧 V1 表不会迁移为 personal Task。当前 V2 schema 不兼容旧 V2 personal 数据库；使用新的 `storage.path` 启动，程序不会自动迁移或删除旧数据库。

V2 Tasks, stage results, budgets, memories, and receipts are saved in the SQLite file at `storage.path`. A Task Run's remaining budget is computed from the Task's cumulative budget when the Run is claimed; it is not stored separately. Interrupted background stages resume with the Task's remaining budget. An interrupted conversation keeps committed actions and reports the interruption; the owner must repeat uncommitted requests, avoiding duplicate task creation. Interrupted sends are marked uncertain. V1 tables are not migrated into personal Tasks. The current V2 schema is incompatible with older V2 personal databases; start with a new `storage.path`. Kagari does not migrate or delete the old database automatically.

`analyze`、`digest` 与 `run` 共用数据库处理器锁，不能同时运行。旧版 `status`、`export` 和重试命令只检查 V1 记录；V2 检查命令只检查 personal 记录。

`analyze`, `digest`, and `run` share the database processor lock and cannot run together. The legacy `status`, `export`, and retry commands inspect V1 records; V2 inspection commands read personal records.

## 数据库维护 / Database maintenance

`db` 命令只打开已存在的数据库，不创建或初始化文件。`info` 和 `list` 以只读模式运行，可在服务期间使用；`list` 的分页参数写在表名之前，默认 20 条，最多 100 条。

The `db` command only opens an existing database; it never creates or initializes one. `info` and `list` use read-only mode and can run while the service is active. Put pagination flags before the table name; the default is 20 rows, with a maximum of 100.

```sh
./bin/kagari -config config.yaml db info
./bin/kagari -config config.yaml db list -limit 20 -offset 0 jobs
./bin/kagari -config config.yaml db list deliveries
./bin/kagari -config config.yaml db list source_cache
./bin/kagari -config config.yaml db list meta
./bin/kagari -config config.yaml db list update_state
```

按用户管理已归档分析。归档是已完成的 `analyze` 任务，归属依据原始提交的 `user_id`。必须显式传 `-user`；CLI 提交未指定用户时归属用户 `0`。列表按任务号从大到小分页，保留重复收录，并输出原始提交和完整分析结果（含来源正文）。分页默认 20 条，最多 100 条；所有选项写在任务号之前。

Manage archived analyses by user. Archives are completed `analyze` jobs, owned by the original submission's `user_id`. An explicit `-user` is required; CLI submissions without a user belong to user `0`. Lists are paginated by descending job ID, preserve duplicate submissions, and include the original submission and complete analysis result with source text. The default page size is 20, with a maximum of 100. Place flags before the task ID.

```sh
./bin/kagari -config config.yaml db archive list -user 123456 -limit 20 -offset 0
./bin/kagari -config config.yaml db archive show -user 123456 17

# 停止处理器后删除指定用户的一条归档 / Stop processors before deleting one archive
./bin/kagari -config config.yaml db archive delete -user 123456 -yes 17
```

`list` 和 `show` 使用只读连接，可在服务运行时执行。`delete` 需要 `-yes` 和处理器锁，会在同一事务中删除匹配用户及任务号的归档和关联投递记录；用户不匹配、任务未完成或任务类型不是分析时拒绝删除。删除后，新建周报的输入不再包含该记录；已有周报及其输入快照、共享来源缓存、旧调度元数据和 V1 Telegram offset 会保留。删除不会撤回已发送的 Telegram 消息；V1 任务号可能重用。

`list` and `show` use read-only connections and can run while the service is active. `delete` requires `-yes` and the processor lock, and removes the archive matching both user and task ID along with its deliveries in one transaction. It rejects an owner mismatch, an incomplete task, or a task that is not an analysis. New digests exclude the deleted record; existing digests and input snapshots, shared source cache, legacy scheduler metadata, and the V1 Telegram offset remain. Deletion does not retract sent Telegram messages; V1 job IDs may be reused.

清理前停止 `run` 或其他处理型命令。`db clear` 只清理 V1 阅读归档表；它不会删除 V2 personal 数据。操作仍需显式传 `-yes`，并会事务性删除所选数据但保留数据库结构：

Stop `run` and other processing commands before clearing data. `db clear` only clears V1 reading-archive tables; it does not delete V2 personal data. Pass `-yes`; the selected deletions are transactional and preserve the database schema:

```sh
# 只清来源缓存 / Clear only the source cache
./bin/kagari -config config.yaml db clear -scope cache -yes

# 默认清任务、投递和来源缓存 / Default: clear jobs, deliveries, and source cache
./bin/kagari -config config.yaml db clear -yes

# 完整重置 V1 数据并清旧调度元数据 / Full reset of V1 data, including legacy scheduler metadata
./bin/kagari -config config.yaml db clear -scope all -yes
```

清理不会撤回已发出的 Telegram 消息；V1 任务号可能被重用。`all` 只重置旧 V1 Telegram offset，personal 服务的 offset、Task 和记忆会保留。

Clearing does not retract sent Telegram messages; V1 job IDs may be reused. `all` resets only the legacy V1 Telegram offset; the personal service's offset, Tasks, and memories remain.

## 行为与限制 / Behavior and limits

- HTTP + Readability 读取网页，限制超时、响应大小、正文长度和链接数，并默认拒绝非公网地址。只有显式配置 `reader.allowed_non_public_cidrs` 才会放行指定网段。 / HTTP + Readability fetches web pages with timeout, response-size, content-length, and link limits. Non-public IPs are blocked by default; only explicitly configured CIDRs are allowed.
- X 通过公开 oEmbed 读取单帖，并提取正文中的裸链接与短链。Agent 自主选择追读，未读内容不能当成已核实证据；oEmbed 可能缺少长帖、线程或链接卡片。 / X oEmbed provides public post text and extracted links. The agent chooses further reads; long posts, threads and link cards may be incomplete.
- `read_url` 通过 HTTP GET 获取网页并提取正文与链接；启用 `browser.enabled` 后，`browse_url` 同时提供 Playwright MCP 的渲染页面读取。模型可选择或混用两个工具，HTTP 返回成功但正文不足时也可改用浏览器。浏览器需要 Node.js 18+ 和本机 Google Chrome；每个任务使用独立无头会话，不继承 Codex 或个人 Chrome 的登录状态。 / `read_url` fetches pages over HTTP GET and extracts text and links. With `browser.enabled`, `browse_url` is also available through Playwright MCP. The model can choose either tool or use both, including when HTTP succeeds but returns insufficient content. Requires Node.js 18+ and local Google Chrome. Each analysis gets an isolated headless browser without personal or Codex login state.
- 默认每次分析最多 6 次不同的 `(URL, 后端)` 读取；首次命中缓存、失败和 HTTP 改用浏览器都计数，同后端同 URL 重读复用本次结果。默认最多 10 次模型迭代、3 分钟任务时限。 / The default is six distinct `(URL, backend)` reads, including cache hits, failures and backend changes; repeated calls reuse the task's observation. Ten model iterations and a three-minute task timeout bound each analysis.
- 周报 `weekly.max_input_chars` 默认 120000；输入超过上限时任务失败并保留完整输入快照，不通过丢弃条目缩小输入。 / `weekly.max_input_chars` defaults to 120000; oversized digest jobs fail with the full input snapshot preserved, rather than dropping entries to fit.
- V1 的 `weekly.enabled` 调度由旧服务使用；V2 `run` 不调度周报。`digest` CLI 可手动生成周报，日期按 `weekly.timezone` 解释。 / The V1 service uses `weekly.enabled` for scheduling; V2 `run` does not schedule digests. The `digest` CLI remains available, with dates interpreted in `weekly.timezone`.

## 日志与隐私 / Logs and privacy

SQLite 保存 owner 私聊、Task 目标与成果、个人记忆及投递记录。它可能包含敏感个人信息和消息历史；妥善保管 `storage.path` 指向的数据库与本地日志。

SQLite stores owner messages, Task goals and results, personal memories, and delivery records. It can contain sensitive personal information and message history; protect the database at `storage.path` and local logs.

本地测试和构建不能代替真实外部联调。模型 endpoint、X 页面和 Telegram bot 需要在目标部署环境分别验证。

Local tests and builds do not replace external integration checks. Validate the model endpoint, X pages, and Telegram bot in the target environment.

## 设计 / Design

V2 的 Task、记忆、恢复边界和 CLI 见[设计说明](docs/V2-DESIGN.md)；V1 阅读归档架构见[旧版设计](docs/DESIGN.md)。

See the [V2 design](docs/V2-DESIGN.md) for Tasks, memory, recovery, and CLI behavior. The [legacy design](docs/DESIGN.md) covers the V1 reading archive.
