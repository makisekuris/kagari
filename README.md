# Kagari

Kagari 是一个 Go 个人阅读助手。它从 CLI 或 Telegram 接收问题、转发文本和链接，由 Agent 自主读取页面、追读相关链接，并选择生成 Markdown 分析或普通对话回复。来源和读取记录保存到 SQLite。周报基于归档分析正文回顾。

Kagari is a personal reading assistant written in Go. It accepts questions, forwarded text and links from the CLI or Telegram. An agent reads relevant pages and follows links, choosing between a Markdown analysis and a conversational reply. Sources and read records are saved to SQLite. Digests review archived analyses.

运行需要 Go 1.27；进程锁使用 `flock`，支持 macOS 和 Linux。模型 endpoint、Telegram bot 和频道权限由部署者配置。

Requires Go 1.27. The process lock uses `flock` and supports macOS and Linux. You provide the model endpoint, Telegram bot, and any channel permissions.

## 日常操作 / Everyday commands

在项目根目录运行 `make` 或 `make help` 查看命令。需要 Go 和 Make；macOS 和 Linux 的 Make 均可使用。

Run `make` or `make help` from the project root to list commands. Requires Go and Make; Make on macOS and Linux is supported.

| 命令 / Command | 用途 / Purpose |
| --- | --- |
| `make init` | 复制示例配置，保留已有文件 / Copy example configuration, preserving existing files |
| `make build` | 为当前平台构建 `bin/kagari` / Build `bin/kagari` for the current platform |
| `make build-mac` | 构建 macOS arm64 产物 / Build for macOS arm64 |
| `make build-linux` | 构建 Linux amd64 产物 / Build for Linux amd64 |
| `make run` | 构建并启动 Telegram 服务 / Build and start the Telegram service |
| `make cli ARGS='...'` | 构建并执行任意 CLI 子命令 / Build and execute any CLI subcommand |
| `make check` | 运行全部测试和 `go vet` / Run all tests and `go vet` |
| `make test` / `make vet` | 单独运行测试或静态检查 / Run tests or static checks separately |
| `make fmt` | 格式化 Go 代码 / Format Go code |

```sh
make cli ARGS='read https://example.com/article'
make cli ARGS='analyze -note "关注系统设计" -user 123456 https://example.com/article'
make cli ARGS='analyze -question "这项设计有哪些取舍？" -user 123456'
make cli ARGS='digest -user 123456'
make cli ARGS='status 17'
make cli ARGS='db info'
make run CONFIG=path/to/config.yaml
```

`run` 和 `cli` 每次先构建，构建失败则停止。`CONFIG` 默认是 `config.yaml`，可在这两个目标上覆盖；`.env` 仍从当前工作目录读取。`ARGS` 按 shell 参数解析，包含空格的单个参数需像上面的 `-note` 一样加引号。其他子命令与数据库清理要求见下文。

`run` and `cli` build first on every invocation and stop if the build fails. `CONFIG` defaults to `config.yaml` and can be overridden for either target; `.env` is still loaded from the current working directory. `ARGS` is parsed as shell arguments; quote individual arguments containing spaces as shown with `-note`. See below for other subcommands and database clearing requirements.

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

在 `.env` 中填写 `KAGARI_MODEL_BASE_URL`、`KAGARI_MODEL_NAME` 和 `KAGARI_MODEL_API_KEY`。使用 Telegram 时填写 `KAGARI_TELEGRAM_TOKEN`，并在 `config.yaml` 的 `telegram.allowed_user_ids` 中加入允许使用 bot 的用户 ID。

Set `KAGARI_MODEL_BASE_URL`, `KAGARI_MODEL_NAME`, and `KAGARI_MODEL_API_KEY` in `.env`. For Telegram, also set `KAGARI_TELEGRAM_TOKEN` and add permitted user IDs to `telegram.allowed_user_ids` in `config.yaml`.

### 2. 构建并试读 / Build and read

```sh
make cli ARGS='read https://example.com/article'
```

`read` 不需要模型凭证。配置好模型后，可分析链接：

`read` does not require model credentials. Configure a model endpoint to analyze a link:

```sh
make cli ARGS='analyze -user 123456 https://example.com/article'
```

启动 Telegram 服务：

Start the Telegram service:

```sh
make run
```

## 配置 / Configuration

所有命令都会读取**当前工作目录**的 `.env`，无需手动 `source`。配置优先级为 **系统环境变量 > .env > YAML > 默认值**；显式设置的空值也会覆盖低优先级配置。`-config` 只指定 YAML 路径，不改变 `.env` 查找目录。

Commands load `.env` from the current working directory; no `source` command is needed. Precedence is **process environment > .env > YAML > defaults**. An explicitly set empty value overrides lower-priority values. `-config` selects the YAML file without changing where `.env` is loaded from.

| 设置 / Setting | 默认值 / Default | 说明 / Description |
| --- | --- | --- |
| `storage.path` | `data/kagari.db` | SQLite 数据库 / SQLite database |
| `profile_path` | `profile.md` | 阅读偏好和表达示例 / Reading preferences and style examples |
| `max_attempts` | `6` | 包含首次尝试 / Includes the first attempt |
| `agent.streaming` | `false` | 启用 Responses SSE / Enables Responses SSE |
| `weekly.enabled` | `false` | 周报定时调度 / Scheduled weekly digests |
| `weekly.max_input_chars` | `120000` | 周报完整模型输入字符上限 / Maximum serialized digest input size |

`.env`、`config.yaml` 和 `profile.md` 已被 Git 忽略。不要将真实凭证写入示例文件或提交到仓库。

`.env`, `config.yaml`, and `profile.md` are Git-ignored. Keep real credentials out of example files and commits.

### 人格替换 / Persona injection

调用方统一通过 `persona.Default()` 获取 `persona.PersonaRole` 接口，再分别注入 Agent 和 Telegram，不直接依赖具体人格类型。角色选择集中在 `internal/persona/main.go`；替换实现只需在这里切换，调用方不变。人格实现提供 `Prompt() string`（人格与语言风格）和 `AskChatID(int64) string`（带任务编号的收件回执），放在 `internal/persona/taffy/` 等子包中；通用任务规则与 Markdown 文风要求保持独立。

Callers obtain the `persona.PersonaRole` interface through `persona.Default()` and inject it into the Agent and Telegram without depending on a concrete persona type. Selection lives in `internal/persona/main.go`; switch implementations there without changing callers. Implementations provide `Prompt() string` for persona and language style, and `AskChatID(int64) string` for job acknowledgements, and live in subpackages such as `internal/persona/taffy/`. Common task rules and Markdown style instructions remain independent.

Engine 构造时读取 `profile.md` 并冻结人格提示词；分析缓存键包含完整分析提示词，周报重试使用任务保存的提示词快照。修改 `profile.md` 后，长期运行的 `run` 服务需重启；单次 `analyze` 和 `digest` 命令会在执行时读取当前文件。修改人格实现后需重新构建程序；运行中的服务还需重启。暂不支持运行时切换角色。

The Engine reads `profile.md` and freezes persona text when constructed. Analysis cache keys include the complete analysis prompt, and digest retries reuse the saved instruction snapshot. Restart a long-running `run` service after changing `profile.md`; one-shot `analyze` and `digest` commands read the current file when executed. Rebuild after changing persona code, then restart a running service to use the new binary. Runtime persona switching is not supported.

### 最终输出 / Final output

分析调用要求模型服务支持 Responses API 的严格 JSON Schema 输出。最终结果只包含 `kind` 和 `body`：`analysis` 的正文是 Markdown 分析，`chat` 的正文是普通对话或澄清问题。程序校验字段、类型和非空正文；无效结果进入任务重试，不发布。正文以外的来源、读取记录和用量由程序保存，周报生成继续直接返回成稿。

Analysis calls require a model endpoint that supports strict JSON Schema output in the Responses API. The final output contains only `kind` and `body`: `analysis` contains a Markdown analysis, while `chat` contains a conversational reply or clarification. The application validates the fields, types, and nonempty body; invalid results enter task retry without publication. Sources, read records, and usage are saved by the application. Digest generation continues to return prose directly.

`analysis` 使用任务入队时保存的分析投递目标；`chat` 只回复提交者私聊，不进入阅读归档或新生成的周报。CLI 没有私聊目标时只展示正文。Telegram 消息格式由渠道适配器负责；当前发送仍为普通文本。

An `analysis` uses the delivery targets saved when the task was queued. A `chat` reply goes only to the submitting private chat and is excluded from the reading archive and newly generated digests. CLI submissions without a private chat target only display the body. The Telegram adapter owns message formatting; delivery currently uses plain text.

## CLI 命令 / CLI commands

全局 `-config` 必须写在子命令之前。以下命令适用于本地构建的 `bin/kagari`：

Place the global `-config` flag before the subcommand. These examples use the locally built `bin/kagari`:

```sh
./bin/kagari -config config.yaml read https://example.com/article
./bin/kagari -config config.yaml analyze -note '关注系统设计' -user 123456 https://example.com/article
./bin/kagari -config config.yaml run
./bin/kagari -config config.yaml digest -user 123456
./bin/kagari -config config.yaml digest -user 123456 -cutoff 2026-10-02T09:00:00+08:00
./bin/kagari -config config.yaml digest -user 123456 -start 2026-09-01 -end 2026-09-08
./bin/kagari -config config.yaml status 17
./bin/kagari -config config.yaml export 17
./bin/kagari -config config.yaml retry 17
./bin/kagari -config config.yaml retry-delivery 17
```

- `read URL` 读取单个 URL，不调用模型，也不创建任务。 / Reads one URL without calling the model or creating a job.
- `analyze [-question TEXT] [-note TEXT] [-user ID] [URL...]` 探索问题或分析链接并归档正文。至少提供问题或链接。 / Explores a question or supplied URLs and archives the prose. Supply a question or a URL.
- `run` 启动 Telegram long polling、任务处理、投递和启用后的周报调度。 / Starts Telegram polling, job processing, delivery, and enabled digest scheduling.
- `digest [-user ID] [-cutoff RFC3339]` 默认回顾截止时间前七个日历日，包含开始时间、不包含截止时间。`-start` 与 `-end` 可指定日期范围，包含开始日期、不包含结束日期；必须同时提供且不能与 `-cutoff` 混用。 / By default, reviews the seven calendar days before the cutoff, including the start and excluding the cutoff. Use `-start` and `-end` together for a date range that includes the start date and excludes the end date; they cannot be combined with `-cutoff`.
- `status ID` 查看任务和投递状态；`export ID` 输出任务 JSON。 / Shows job and delivery status; `export ID` prints the job JSON.
- `retry ID` 重新排队失败任务；`retry-delivery ID` 手动重发失败或结果不确定的投递。 / Requeues a failed job; manually retries a failed or uncertain delivery.

未指定 `-cutoff`、`-start` 和 `-end` 时，周报使用执行命令时的当前时间作为截止时间。同一用户、版本和精确时间范围可复用已保存结果。空周报无需调用模型。默认总共最多尝试 6 次，包含首次尝试；中间失败不会向 Telegram 发送最终失败通知。

Without `-cutoff`, `-start`, or `-end`, a digest uses the current time when the command runs as its cutoff. The same user, version, and exact time range can reuse a saved result. Empty digests need no model call. The default maximum is six attempts, including the first; intermediate failures do not send a final Telegram notice.

CLI 的 `analyze` 和 `digest` 会启动任务处理器，不能与 `run` 或另一个处理型 CLI 命令同时运行。状态、导出和重试命令可以在服务运行时使用。

CLI `analyze` and `digest` start a processor and must not run alongside `run` or another processing CLI command. Status, export, and retry commands can run while the service is active.

Telegram 只接收允许列表中用户的私聊消息。确认、命令回复和处理失败通知发回提交私聊。分析与周报结果默认发回提交私聊；配置非空的 `telegram.target_chat_ids` 后，同一结果会分发到列表中的所有聊天或频道，bot 需要相应发言权限。列表为空时仍回发提交私聊。支持 `/help`、`/status ID`、`/retry ID`、`/retry_delivery ID`、`/weekly`、`/weekly START END` 和下述归档命令。

Telegram accepts private messages from users on the allowlist. Acknowledgements, command replies, and processing failure notices go to the submitting private chat. Analysis and digest results default to the submitting chat; a nonempty `telegram.target_chat_ids` list sends each result to every listed chat or channel, where the bot needs posting permission. An empty list keeps results in the submitting private chat. Supported commands include `/help`, `/status ID`, `/retry ID`, `/retry_delivery ID`, `/weekly`, `/weekly START END`, and the archive commands below.

私聊 bot 可分页查看自己的归档、读取详情并确认删除。用户身份取自 Telegram 发送者，不接受用户 ID 参数。

In a private bot chat, browse your archives, read details, and confirm deletion. Ownership comes from the Telegram sender; commands do not accept a user ID.

```text
/archive
/archive 2
/archive_show 17
/archive_delete 17
/archive_delete 17 confirm
```

`/archive [页码]` 默认第一页，每页 10 条，按任务号从大到小列出已完成分析，保留重复收录；收录日期使用 `weekly.timezone`。`/archive_show` 展示原始提交、备注、链接和完整分析报告。`/archive_delete ID` 先展示归档标题和删除范围，带 `confirm` 的命令才执行删除。正在发送消息的归档会拒绝删除，请等待投递结束后重试。删除范围与 CLI 相同：删除归档及关联投递记录，保留共享缓存和已有周报快照，也不会撤回已发送消息。

`/archive [PAGE]` defaults to page 1, with 10 completed analyses per page in descending job ID order, including duplicate submissions; submission dates use `weekly.timezone`. `/archive_show` displays the original submission, notes, links, and full analysis report. `/archive_delete ID` first displays the archive title and deletion scope; deletion requires the command with `confirm`. An archive with an active message send cannot be deleted; retry after delivery finishes. As with the CLI, deletion removes the archive and its deliveries while preserving shared cache and existing digest snapshots, and does not retract sent messages.

归档命令使用现有任务队列和投递流程，因此分析任务执行时命令回复可能延迟。回复发回发起命令的私聊，不使用分析结果的公共投递目标。

Archive commands use the existing job queue and delivery flow, so an analysis in progress may delay replies. Replies go to the private chat where the command was sent, rather than the public analysis delivery target.

```yaml
telegram:
  allowed_user_ids: [123456789]
  target_chat_ids: [123456789, -1001234567890]
```

用实际私聊和频道 ID 替换示例；也可设置 `KAGARI_TELEGRAM_TARGET_CHAT_IDS=123456789,-1001234567890`。目标在入队时保存，修改配置只影响新任务。每个目标分别记录投递状态和分段顺序，一个目标失败不会阻塞其他目标。`/status ID` 显示每个目标的状态；`/retry_delivery ID` 仅重新排队失败或结果不确定的投递，已发送部分保留。结果不确定的消息仍需手动重发，可能重复。

Replace the example IDs with your private chat and channel IDs, or set `KAGARI_TELEGRAM_TARGET_CHAT_IDS=123456789,-1001234567890`. Targets are saved at enqueue time; configuration changes affect new jobs. Each target has independent delivery status and part ordering, so one target's failure does not block the others. `/status ID` shows each target's status. `/retry_delivery ID` requeues only failed or uncertain deliveries and preserves sent parts; retrying an uncertain delivery may duplicate a message.

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

`list` 和 `show` 使用只读连接，可在服务运行时执行。`delete` 需要 `-yes` 和处理器锁，会在同一事务中删除匹配用户及任务号的归档和关联投递记录；用户不匹配、任务未完成或任务类型不是分析时拒绝删除。删除后，新建周报的输入不再包含该记录；已有周报及其输入快照、共享来源缓存、调度元数据和 Telegram offset 会保留。删除不会撤回已发送的 Telegram 消息；任务号可能重用。

`list` and `show` use read-only connections and can run while the service is active. `delete` requires `-yes` and the processor lock, and removes the archive matching both user and task ID along with its deliveries in one transaction. It rejects an owner mismatch, an incomplete task, or a task that is not an analysis. New digests exclude the deleted record; existing digests and input snapshots, shared source cache, scheduler metadata, and Telegram offset remain. Deletion does not retract sent Telegram messages; task IDs may be reused.

清理前停止 `run` 或其他处理型命令。清理需要显式传 `-yes`，会事务性删除数据但保留数据库结构：

Stop `run` and other processing commands before clearing data. Clearing requires `-yes`; deletions are transactional and preserve the database schema:

```sh
# 只清来源缓存 / Clear only the source cache
./bin/kagari -config config.yaml db clear -scope cache -yes

# 默认清任务、投递和来源缓存 / Default: clear jobs, deliveries, and source cache
./bin/kagari -config config.yaml db clear -yes

# 完整重置并清调度元数据 / Full reset, including scheduler metadata
./bin/kagari -config config.yaml db clear -scope all -yes
```

清理不会撤回已发出的 Telegram 消息；清任务后任务号可能被重用。`all` 会重置 Telegram offset，尚未确认的更新可能重新入队。

Clearing does not retract Telegram messages already sent. Job IDs may be reused after jobs are deleted. `all` resets the Telegram offset, so unconfirmed updates may be enqueued again.

## 行为与限制 / Behavior and limits

- HTTP + Readability 读取网页，限制超时、响应大小、正文长度和链接数，并默认拒绝非公网地址。只有显式配置 `reader.allowed_non_public_cidrs` 才会放行指定网段。 / HTTP + Readability fetches web pages with timeout, response-size, content-length, and link limits. Non-public IPs are blocked by default; only explicitly configured CIDRs are allowed.
- X 通过公开 oEmbed 读取单帖，并提取正文中的裸链接与短链。Agent 自主选择追读，未读内容不能当成已核实证据；oEmbed 可能缺少长帖、线程或链接卡片。 / X oEmbed provides public post text and extracted links. The agent chooses further reads; long posts, threads and link cards may be incomplete.
- `read_url` 通过 HTTP GET 获取网页并提取正文与链接；启用 `browser.enabled` 后，`browse_url` 同时提供 Playwright MCP 的渲染页面读取。模型可选择或混用两个工具，HTTP 返回成功但正文不足时也可改用浏览器。浏览器需要 Node.js 18+ 和本机 Google Chrome；每个任务使用独立无头会话，不继承 Codex 或个人 Chrome 的登录状态。 / `read_url` fetches pages over HTTP GET and extracts text and links. With `browser.enabled`, `browse_url` is also available through Playwright MCP. The model can choose either tool or use both, including when HTTP succeeds but returns insufficient content. Requires Node.js 18+ and local Google Chrome. Each analysis gets an isolated headless browser without personal or Codex login state.
- 默认每次分析最多 6 次不同的 `(URL, 后端)` 读取；首次命中缓存、失败和 HTTP 改用浏览器都计数，同后端同 URL 重读复用本次结果。默认最多 10 次模型迭代、3 分钟任务时限。 / The default is six distinct `(URL, backend)` reads, including cache hits, failures and backend changes; repeated calls reuse the task's observation. Ten model iterations and a three-minute task timeout bound each analysis.
- 周报 `weekly.max_input_chars` 默认 120000；输入超过上限时任务失败并保留完整输入快照，不通过丢弃条目缩小输入。 / `weekly.max_input_chars` defaults to 120000; oversized digest jobs fail with the full input snapshot preserved, rather than dropping entries to fit.
- `weekly.enabled` 默认关闭；启用后按 `weekly.timezone` 和配置的星期、时间调度，默认时区为 `Asia/Shanghai`。 / `weekly.enabled` defaults to false. When enabled, digests follow `weekly.timezone` and the configured weekday and time; the default timezone is `Asia/Shanghai`.

## 日志与隐私 / Logs and privacy

启用 `agent.streaming` 后，通过 SSE 接收模型输出；逐段 SSE 日志目前关闭。info 日志记录模型轮次、完整工具参数、工具结果摘要和 token 用量；非流式模式还会记录完整模型回复。日志可能包含提交内容或提取文本。SQLite 归档也包含提交和来源正文，请妥善保管本地日志和数据库。

With `agent.streaming` enabled, model output is received over SSE; per-chunk SSE logs are currently disabled. Info logs record model turns, complete tool arguments, tool result previews, and token usage. Non-streaming mode also logs the full model response. Logs may contain submitted text or extracted content. SQLite archives contain submissions and source text; protect local logs and database files.

本地测试和构建不能代替真实外部联调。模型 endpoint、X 页面和 Telegram bot 需要在目标部署环境分别验证。

Local tests and builds do not replace external integration checks. Validate the model endpoint, X pages, and Telegram bot in the target environment.

## 设计 / Design

架构、数据流、可靠性和安全契约见[设计说明](docs/DESIGN.md)。

See the [design document](docs/DESIGN.md) for architecture, data flows, and reliability and security contracts.
