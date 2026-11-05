# Kagari

Kagari 是一个 Go 个人 Agent。`kagari run` 启动面向单一使用者的 Telegram 或微信私聊服务，持续推进后台任务并维护长期记忆。

Kagari is a personal agent written in Go. `kagari run` starts a Telegram or Weixin private-chat service for one owner, continuing background tasks and maintaining long-term memory.

运行需要 Go 1.27；进程锁使用 `flock`，支持 macOS 和 Linux。部署者提供模型 endpoint，并配置 Telegram bot 或扫码绑定微信；服务只向绑定的使用者私聊回复。

Requires Go 1.27. The process lock uses `flock` and supports macOS and Linux. You provide the model endpoint and configure Telegram or sign in to Weixin; the service replies only to the bound owner in private chat.

## 日常操作 / Everyday commands

在项目根目录运行 `make` 或 `make help` 查看命令。需要 Go 和 Make；macOS 和 Linux 的 Make 均可使用。

Run `make` or `make help` from the project root to list commands. Requires Go and Make; Make on macOS and Linux is supported.

| 命令 / Command | 用途 / Purpose |
| --- | --- |
| `make init` | 复制示例配置，保留已有文件 / Copy example configuration, preserving existing files |
| `make build` | 为当前平台构建 `bin/kagari` / Build `bin/kagari` for the current platform |
| `make build-mac` | 构建 macOS arm64 产物 / Build for macOS arm64 |
| `make build-linux` | 构建 Linux amd64 产物 / Build for Linux amd64 |
| `make run` | 构建并启动所选 personal 渠道 / Build and start the selected personal channel |
| `make cli ARGS='...'` | 构建并执行任意 CLI 子命令 / Build and execute any CLI subcommand |
| `make check` | 运行全部测试和 `go vet` / Run all tests and `go vet` |
| `make test` / `make vet` | 单独运行测试或静态检查 / Run tests or static checks separately |
| `make fmt` | 格式化 Go 代码 / Format Go code |

GitHub Actions 在每次 push 和 PR 上运行 `make check`，Go 版本取自 `go.mod`。

GitHub Actions runs `make check` on pushes and pull requests, using the Go version in `go.mod`.

```sh
make cli ARGS='tasks'
make cli ARGS='tasks 1'
make cli ARGS='runs 1'
make cli ARGS='memories "answer style"'
make cli ARGS='memory-history answer_style'
make run CONFIG=path/to/config.yaml
```

`run` 和 `cli` 每次先构建，构建失败则停止。`CONFIG` 默认是 `config.yaml`，可在这两个目标上覆盖；`.env` 仍从当前工作目录读取。`ARGS` 按 shell 参数解析，包含空格的单个参数需像上面的 `memories` 查询一样加引号。检查命令输出 JSON。

`run` and `cli` build first on every invocation and stop if the build fails. `CONFIG` defaults to `config.yaml` and can be overridden for either target; `.env` is still loaded from the current working directory. `ARGS` is parsed as shell arguments; quote individual arguments containing spaces as shown with the `memories` query. Inspection commands print JSON.

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

在 `.env` 中填写 `KAGARI_MODEL_BASE_URL`、`KAGARI_MODEL_NAME`、`KAGARI_MODEL_API_KEY` 和 `KAGARI_TELEGRAM_TOKEN`。在 `config.yaml` 中填入唯一一个正数 `telegram.allowed_user_ids`；`run` 不支持多个 owner 或群聊目标。

Set `KAGARI_MODEL_BASE_URL`, `KAGARI_MODEL_NAME`, `KAGARI_MODEL_API_KEY`, and `KAGARI_TELEGRAM_TOKEN` in `.env`. In `config.yaml`, set exactly one positive `telegram.allowed_user_ids` entry; `run` does not support multiple owners or group destinations.

```yaml
telegram:
  allowed_user_ids: [123456789]

personal:
  task_concurrency: 4
  task_budget: 30m
```

### 微信与附件 / Weixin and attachments

将 `personal.channel` 设为 `weixin`，使用新的 `storage.path`，然后构建、扫码并运行。微信模式不要求 Telegram token 或用户 ID。`login` 不调用模型；`run` 仍需要模型配置。

Set `personal.channel: weixin` and choose a new `storage.path`. Weixin does not require Telegram credentials. Login does not call the model; the service still needs model configuration.

```sh
make build
./bin/kagari -config config.yaml weixin login -qr /tmp/kagari-login.png
./bin/kagari -config config.yaml channel status
./bin/kagari -config config.yaml run
```

二维码在终端显示；`-qr` 可选，图片路径须不存在。二维码过期后该图片会更新。同一微信用户重新 `login` 后，运行中的服务自动重载凭证；更换用户须先停止服务，再执行 `weixin rebind`。登录 token 更新后，请向 Bot 发一条新消息以恢复待发送结果。凭证保存在 `<storage.path>.channels/weixin.json`。状态命令展示本地配置与已保存账号，不代表平台当前可用。

The terminal displays the QR code. Optional `-qr` saves and refreshes a PNG at a previously unused path. Repeating `login` for the same user reloads credentials in the running service; changing users requires stopping the service and running `weixin rebind`. After a login token changes, send a new message to the bot to resume pending deliveries. Credentials are stored at `<storage.path>.channels/weixin.json`. Status reports saved configuration, not live platform availability.

两渠道支持文字和普通文件，单文件双向上限 20,000,000 字节；请将图片作为文件发送。Agent 收到正文与附件 ID，可读取最多 1 MiB 的 UTF-8 文本、生成文本成果并选择发送；本次未增加 PDF、Office、音视频解析器。附件保留至显式删除：

Both channels support text and ordinary files up to 20,000,000 bytes each; send images as files. The Agent receives text and attachment IDs, reads UTF-8 text up to 1 MiB, and selects generated text files for delivery. PDF, Office, audio and video parsing are not included. Files remain until explicitly deleted:

```sh
./bin/kagari attachments list
./bin/kagari attachments delete ATTACHMENT_ID
```

切换渠道需停止服务、修改 `personal.channel` 并使用同一新结构数据库重启。对话、任务、记忆和 persona 共享，待发送结果走当前渠道；成功或结果不确定的投递不会自动重发。旧 personal 数据库不迁移也不自动删除。删除附件保留既有分析与记忆，也不撤回已发送文件。更多细节见[渠道设计](docs/CHANNEL-DESIGN.md)。

Stop the service, change `personal.channel`, and restart with the same new-schema database to switch channels. Conversations, tasks, memory and persona are shared. Pending results use the active channel; sent or uncertain deliveries are not replayed automatically. Older personal databases are neither migrated nor deleted. Deleting a file preserves analysis and memory and does not recall remote copies. See the [channel design](docs/CHANNEL-DESIGN.md).

### 2. 启动服务 / Start the service

启动所选渠道后，在已绑定账号的私聊中直接发送任务或偏好：

Start the selected channel, then send a task or preference in the bound owner's private chat:

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
| `agent.streaming` | `false` | 启用 Responses SSE / Enables Responses SSE |
| `personal.task_concurrency` | `4` | 并行 Task worker 数，必须为正数 / Concurrent task workers; must be positive |
| `personal.task_budget` | `30m` | 每个 Task 的累计执行时长上限 / Total execution time limit per Task |

`.env`、`config.yaml` 和 `profile.md` 已被 Git 忽略。不要将真实凭证写入示例文件或提交到仓库。

`.env`, `config.yaml`, and `profile.md` are Git-ignored. Keep real credentials out of example files and commits.

### 人格替换 / Persona injection

V2 `run` 通过 `persona.Default()` 读取人格提示词，并在启动时载入 `profile.md`。角色选择集中在 `internal/persona/main.go`；替换实现后重新构建并重启服务，修改 `profile.md` 后重启服务。

V2 `run` reads the persona prompt through `persona.Default()` and loads `profile.md` at startup. Persona selection lives in `internal/persona/main.go`; rebuild and restart after changing the implementation, and restart after changing `profile.md`.

内置永雏塔菲（默认）和明前奶绿。使用奶绿时，将 `internal/persona/main.go` 的导入改为 `nailv "kagari/internal/persona/nailv"`，并在 `Default()` 中返回 `nailv.Nailv{}`。奶绿人格参考[公开角色资料](https://laplace.live/about)，采用花店店员的背景和亲切、慵懒、偶尔吐槽的语气。

Built-in personas are Taffy (default) and Mingqian Nailv. To use Nailv, change the import in `internal/persona/main.go` to `nailv "kagari/internal/persona/nailv"` and return `nailv.Nailv{}` from `Default()`. Nailv draws on the [public character profile](https://laplace.live/about), using a flower shop assistant background and a friendly, laid-back tone with occasional teasing.

### 模型输出 / Model output

主对话和读取阶段在调用工具时不附加 JSON Schema 输出约束，最终结果由程序校验。主对话格式不合格时，只额外进行一次无工具的严格格式整理，不重做操作；自动记忆提取也使用严格 JSON Schema，因此模型端点仍需支持该能力。Task 执行器直接返回文字摘要。

Conversation and reading tool loops omit wire-level JSON Schema and validate the final result locally. A malformed conversation reply gets one tool-free formatting attempt without replaying actions. Automatic memory extraction also uses strict JSON Schema, so the endpoint must still support it. The Task executor returns a text summary directly.

## CLI 命令 / CLI commands

全局 `-config` 必须写在子命令之前。以下命令适用于本地构建的 `bin/kagari`：

Place the global `-config` flag before the subcommand. These examples use the locally built `bin/kagari`:

```sh
./bin/kagari -config config.yaml tasks
./bin/kagari -config config.yaml tasks 1
./bin/kagari -config config.yaml runs 1
./bin/kagari -config config.yaml memories 'answer style'
./bin/kagari -config config.yaml memory-history answer_style
```

- V2 `tasks [TASK_ID]` 输出最近 Task 或单个 Task 的 JSON；`runs TASK_ID` 输出该 Task 的 Run 记录。
- V2 `memories [QUERY]` 以不区分大小写的子串搜索当前有效的记忆；省略查询时列出最近记忆。`memory-history [KEY]` 输出最近记忆变更；传入 KEY 时精确筛选该键，历史中可能包含已遗忘前的值。这些命令只读 personal 表，不恢复或修改服务状态。

V2 `tasks [TASK_ID]` prints recent Tasks or one Task as JSON; `runs TASK_ID` prints that Task's Run records. `memories [QUERY]` performs a case-insensitive substring search over active memory keys and values; omitting QUERY lists recent memories. `memory-history [KEY]` prints recent memory changes, optionally filtered by an exact key. History can include values from before a memory was forgotten. These commands read personal tables without recovering or changing service state.

V2 通过 Telegram owner 私聊接收任务、取消或继续任务，也可自然表达稳定偏好、纠正和遗忘请求。例如：

V2 accepts tasks and requests to cancel or continue them in the owner's Telegram private chat. Stable preferences, corrections, and forgetting can also be phrased naturally. For example:

```text
调查 https://example.com 的方案和主要风险。
这个任务进展如何？
取消任务 #1。
继续任务 #1，补查兼容性；我授权按当前配置延长一次执行时间。
以后回答请简洁一些，但保留关键依据。
更正：我现在主要使用 Rust。
忘记我偏好简洁回答这件事。
```

每条直接用户消息处理后都会异步检查稳定事实和偏好。自动记忆检查始终静默，包括失败和需要澄清的情况；结果与错误保留在 Run 中，查询个人记忆可确认已生效的内容。用户明确要求保存、更正或遗忘时，主对话会同步提交记忆、变更历史和请求记录；成功后才可报告已生效，校验或版本冲突会作为工具错误返回。

After each direct user message, a background reviewer checks for durable facts and preferences. Automatic memory review stays silent, including failures and clarification; results and errors remain in Runs. When the user explicitly asks to save, correct, or forget a memory, the conversation synchronously commits the memory, its change history, and the request record. It can report success only after the commit; validation and version conflicts return as tool errors.

两个渠道的文字回复均使用普通文本，每条 outbox 分段最多 2,000 字节，按 UTF-8 字符边界切分；每段或每个文件总共最多尝试三次，包含首次尝试。发送结果未知时不会自动重发，避免重复消息。

Both channels use plain text replies, split into outbox parts of at most 2,000 bytes at UTF-8 character boundaries. Each text part or file gets at most three total attempts, including the first. An unknown send outcome is not retried automatically, to avoid duplicate messages.

V2 的 Task 状态、阶段成果、已用执行时间与时长上限、记忆和回执保存在 `storage.path` 指向的 SQLite 文件中。Worker 领取 Task Run 时，用时长上限减去该 Task 已用的执行时间，得到本次可用的时间；这个计算结果不单独保存。服务重启后，中断的后台阶段重新排队，已用时间不会清零。中断的主对话保留已经提交的操作并说明中断，未提交的要求需要重新提出，以免重复建任务。发送中断的消息标记为结果不确定；旧 V1 表不会迁移为 personal Task。当前 V2 schema 不兼容旧 V2 personal 数据库；使用新的 `storage.path` 启动，程序不会自动迁移或删除旧数据库。

V2 Task states, stage results, elapsed execution times and time limits, memories, and receipts are saved in the SQLite file at `storage.path`. When a worker claims a Task Run, it subtracts the Task's elapsed execution time from its time limit to find the time available for that Run. This calculated value is not stored separately. Restarting the service requeues interrupted background stages without resetting elapsed time. An interrupted conversation keeps committed actions and reports the interruption; the owner must repeat uncommitted requests, avoiding duplicate task creation. Interrupted sends are marked uncertain. V1 tables are not migrated into personal Tasks. The current V2 schema is incompatible with older V2 personal databases; start with a new `storage.path`. Kagari does not migrate or delete the old database automatically.

Task 执行器可以读取网页，也可以用 `ls`、`read_file`、`write_file`、`edit_file` 读写当前 Task 工作区内的 UTF-8 文本文件，每个文件最多 1 MiB。执行器还可用 `read_attachment` 读取选定材料、用 `write_attachment` 保存独立文本附件；不能任意写入本地路径、执行命令或修改外部服务。

The Task executor can read web pages and use `ls`, `read_file`, `write_file`, and `edit_file` to read and write UTF-8 text files in the current Task's workspace, up to 1 MiB per file. It can also use `read_attachment` for selected materials and `write_attachment` to save independent text attachments. It cannot write arbitrary local paths, execute commands, or modify external services.

每个 Task 的文件保存在 `<storage.path>.workspaces/<store_id>/tasks/<task_id>/`。继续任务或重启服务后，执行器仍使用这个目录。`edit_file` 要求待替换文本在文件中恰好出现一次。文件写入成功后就会保留，即使阶段随后失败或取消，也不会撤销修改。Run 的 `files` 列出本次成功写入或修改的文件相对路径；这些路径指向当前文件，不保存历史版本，也不表示文件已作为附件发送。选定文本成果需通过 `write_attachment` 保存，再由主 Agent 选择附件 ID 发送；工作目录文件不会自动转为附件。备份时需同时保留数据库、工作区、附件和凭证。

Each Task stores its files at `<storage.path>.workspaces/<store_id>/tasks/<task_id>/`. The executor uses the same directory when the Task continues or the service restarts. `edit_file` requires the text to replace to appear exactly once. A successful write is kept even if the stage later fails or is cancelled. A Run's `files` field lists the relative paths of files it successfully wrote or modified. These paths refer to the current files; they do not preserve older versions or indicate that the files were sent as attachments. Save selected text deliverables with `write_attachment`; the main Agent chooses attachment IDs to send. Workspace files are not automatically exported. Back up the database, workspace, attachments, and credentials together.

同一个数据库只能由一个 `run` 进程持有锁；只读检查命令可在服务运行时使用。

Only one `run` process can hold a database's lock. Read-only inspection commands remain available while the service runs.

### 从旧版本更新 / Updating from older versions

V1 阅读归档及其 `read`、`analyze`、`digest`、`status`、`export`、`retry`、`retry-delivery`、`db` 命令已移除。旧数据库和旧表不会自动删除，也不会导入 personal Task。现有 personal 数据库可继续使用，本次清理不改变其 schema。

V1 reading archives and the `read`, `analyze`, `digest`, `status`, `export`, `retry`, `retry-delivery`, and `db` commands have been removed. Old databases and tables are neither deleted automatically nor imported into personal Tasks. Existing personal databases remain usable; this cleanup does not change their schema.

从旧配置删除 `weekly`、`max_attempts`、`reader.cache_ttl` 和 `telegram.target_chat_ids`，以及对应的 `KAGARI_*` 环境变量。YAML 仍严格校验，保留这些旧键会报错。网页读取只在同一次 Task 阶段内复用 HTTP 结果。

Remove `weekly`, `max_attempts`, `reader.cache_ttl`, and `telegram.target_chat_ids`, plus their corresponding `KAGARI_*` environment variables. YAML validation remains strict and rejects these removed keys. HTTP results are reused only within one Task stage.

## 行为与限制 / Behavior and limits

- HTTP + Readability 读取网页，限制超时、响应大小、正文长度和链接数，并默认拒绝非公网地址。只有显式配置 `reader.allowed_non_public_cidrs` 才会放行指定网段。 / HTTP + Readability fetches web pages with timeout, response-size, content-length, and link limits. Non-public IPs are blocked by default; only explicitly configured CIDRs are allowed.
- X 通过公开 oEmbed 读取单帖，并提取正文中的裸链接与短链。Agent 自主选择追读，未读内容不能当成已核实证据；oEmbed 可能缺少长帖、线程或链接卡片。 / X oEmbed provides public post text and extracted links. The agent chooses further reads; long posts, threads and link cards may be incomplete.
- `read_url` 通过 HTTP GET 获取网页并提取正文与链接；启用 `browser.enabled` 后，`browse_url` 同时提供 Playwright MCP 的渲染页面读取。模型可选择或混用两个工具，HTTP 返回成功但正文不足时也可改用浏览器。浏览器需要 Node.js 18+ 和本机 Google Chrome；每个 Task 阶段使用独立无头会话，不继承 Codex 或个人 Chrome 的登录状态。 / `read_url` fetches pages over HTTP GET and extracts text and links. With `browser.enabled`, `browse_url` is also available through Playwright MCP. The model can choose either tool or use both, including when HTTP succeeds but returns insufficient content. Requires Node.js 18+ and local Google Chrome. Each Task stage gets an isolated headless browser without personal or Codex login state.
- 默认每个 Task 阶段最多 6 次不同的 `(URL, 后端)` 读取；失败和 HTTP 改用浏览器都计数，同后端同 URL 重读复用本次结果。默认最多 10 次模型迭代、3 分钟阶段时限。 / The default is six distinct `(URL, backend)` reads, including failures and backend changes; repeated calls reuse the task's observation. Ten model iterations and a three-minute timeout bound each Task stage.

## 日志与隐私 / Logs and privacy

V2 `make run` 在 stderr 显示阶段、关联 ID、耗时和结果摘要；应用将完整调用记录保存为 JSONL。默认 `data/kagari.db` 对应 `data/kagari.logs/`，按约 50 MiB 轮转，保留最近 7 天且总量不超过 1 GiB，先触发的限制生效。`logging` 配置可修改这些值；V2 的 `log_level` 只调整终端阈值，文件始终保留 info 及以上记录。

V2 writes concise progress to stderr and full application call records to JSONL files. For `data/kagari.db`, the default directory is `data/kagari.logs/`. Files rotate around 50 MiB and expire after 7 days or when total storage reaches 1 GiB, whichever comes first. Configure these values under `logging`; V2 `log_level` controls the terminal threshold while files retain info and above.

```sh
./bin/kagari -config config.yaml events --limit 20
./bin/kagari -config config.yaml trace --event 42
./bin/kagari -config config.yaml trace --task 3 --full
```

`events` 提供近期事件 ID；`trace` 将关联日志与数据库当前状态放在一起，默认隐藏正文，`--full` 显示脱敏后的调用内容。查询使用只读数据库连接，不启动服务或恢复工作。记录范围、故障边界和验证依据见[观测性说明](docs/V2-OBSERVABILITY.md)。

`events` lists recent event IDs. `trace` combines related historical records with current database state; `--full` includes redacted payloads. Inspection uses a read-only connection and does not start workers or recover work. See the [observability guide](docs/V2-OBSERVABILITY.md) for coverage, failure behavior, and test rationale.

SQLite 保存 owner 私聊、Task 目标与成果、个人记忆及投递记录。它可能包含敏感个人信息和消息历史；妥善保管 `storage.path` 指向的数据库与本地日志。

SQLite stores owner messages, Task goals and results, personal memories, and delivery records. It can contain sensitive personal information and message history; protect the database at `storage.path` and local logs.

文件日志默认保留聊天正文、模型上下文与响应、工具定义、参数和结果。已配置的模型密钥、Telegram token 和明确的凭证字段会被屏蔽；普通正文中未知的秘密仍可能保留。日志目录权限为 0700、文件为 0600；请使用专用日志目录。

File logs retain message bodies, model context and responses, tool definitions, arguments, and results. Configured model and Telegram credentials and explicit credential fields are redacted; unknown secrets in ordinary prose may remain. Log directories use mode 0700 and files 0600. Use a dedicated log directory.

本地测试和构建不能代替真实外部联调。模型 endpoint、X 页面和 Telegram bot 需要在目标部署环境分别验证。

Local tests and builds do not replace external integration checks. Validate the model endpoint, X pages, and Telegram bot in the target environment.

## 设计 / Design

Task、记忆、恢复边界和 CLI 见[设计说明](docs/V2-DESIGN.md)。

See the [design](docs/V2-DESIGN.md) for Tasks, memory, recovery, and CLI behavior.
