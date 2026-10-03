# Kagari

Kagari 是一个 Go 个人阅读助手。它从 CLI 或 Telegram 接收链接，读取网页与公开 X 单帖，生成带来源依据的分析，并保存到本地 SQLite。周报 Agent 可以回顾已保存的分析、合并重复内容并按分类整理。

Kagari is a personal reading assistant written in Go. It accepts links from the CLI or Telegram, reads web pages and public X posts, produces source-backed analysis, and stores it in local SQLite. A weekly digest agent can review saved analyses, merge duplicates, and group them by category.

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
| `agent.streaming` | `false` | 启用 Responses SSE 与实时日志 / Enables Responses SSE and live logs |
| `weekly.enabled` | `false` | 周报定时调度 / Scheduled weekly digests |
| `weekly.max_input_chars` | `120000` | 周报完整模型输入字符上限 / Maximum serialized digest input size |

`.env`、`config.yaml` 和 `profile.md` 已被 Git 忽略。不要将真实凭证写入示例文件或提交到仓库。

`.env`, `config.yaml`, and `profile.md` are Git-ignored. Keep real credentials out of example files and commits.

### 人格替换 / Persona injection

调用方统一通过 `persona.Default()` 获取 `persona.PersonaRole` 接口，再分别注入 Agent 和 Telegram，不直接依赖具体人格类型。角色选择集中在 `internal/persona/main.go`；替换实现只需在这里切换，调用方不变。人格实现提供 `Prompt() string`（人格与语言风格）和 `AskChatID(int64) string`（带任务编号的收件回执），放在 `internal/persona/taffy/` 等子包中；通用任务规则和 JSON 输出格式保持独立。

Callers obtain the `persona.PersonaRole` interface through `persona.Default()` and inject it into the Agent and Telegram without depending on a concrete persona type. Selection lives in `internal/persona/main.go`; switch implementations there without changing callers. Implementations provide `Prompt() string` for persona and language style, and `AskChatID(int64) string` for job acknowledgements, and live in subpackages such as `internal/persona/taffy/`. Common task rules and the JSON output format remain independent.

Engine 构造时读取 `profile.md` 并冻结人格提示词；分析缓存键包含完整分析提示词，周报重试使用任务保存的提示词快照。修改 `profile.md` 后，长期运行的 `run` 服务需重启；单次 `analyze` 和 `digest` 命令会在执行时读取当前文件。修改人格实现后需重新构建程序；运行中的服务还需重启。暂不支持运行时切换角色。

The Engine reads `profile.md` and freezes persona text when constructed. Analysis cache keys include the complete analysis prompt, and digest retries reuse the saved instruction snapshot. Restart a long-running `run` service after changing `profile.md`; one-shot `analyze` and `digest` commands read the current file when executed. Rebuild after changing persona code, then restart a running service to use the new binary. Runtime persona switching is not supported.

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
- `analyze [-note TEXT] [-user ID] URL...` 分析一个或多个链接并归档结果。 / Analyzes one or more URLs and archives the result.
- `run` 启动 Telegram long polling、任务处理、投递和启用后的周报调度。 / Starts Telegram polling, job processing, delivery, and enabled digest scheduling.
- `digest [-user ID] [-cutoff RFC3339]` 默认回顾截止时间前七个日历日，包含开始时间、不包含截止时间。`-start` 与 `-end` 可指定日期范围，包含开始日期、不包含结束日期；必须同时提供且不能与 `-cutoff` 混用。 / By default, reviews the seven calendar days before the cutoff, including the start and excluding the cutoff. Use `-start` and `-end` together for a date range that includes the start date and excludes the end date; they cannot be combined with `-cutoff`.
- `status ID` 查看任务和投递状态；`export ID` 输出任务 JSON。 / Shows job and delivery status; `export ID` prints the job JSON.
- `retry ID` 重新排队失败任务；`retry-delivery ID` 手动重发失败或结果不确定的投递。 / Requeues a failed job; manually retries a failed or uncertain delivery.

未指定 `-cutoff`、`-start` 和 `-end` 时，周报使用执行命令时的当前时间作为截止时间。同一用户、版本和精确时间范围可复用已保存结果。空周报无需调用模型。默认总共最多尝试 6 次，包含首次尝试；中间失败不会向 Telegram 发送最终失败通知。

Without `-cutoff`, `-start`, or `-end`, a digest uses the current time when the command runs as its cutoff. The same user, version, and exact time range can reuse a saved result. Empty digests need no model call. The default maximum is six attempts, including the first; intermediate failures do not send a final Telegram notice.

CLI 的 `analyze` 和 `digest` 会启动任务处理器，不能与 `run` 或另一个处理型 CLI 命令同时运行。状态、导出和重试命令可以在服务运行时使用。

CLI `analyze` and `digest` start a processor and must not run alongside `run` or another processing CLI command. Status, export, and retry commands can run while the service is active.

Telegram 只接收允许列表中用户的私聊消息。确认、命令回复和处理失败通知发回私聊；分析与周报结果默认发回提交私聊。配置 `telegram.target_chat_ids` 可将同一结果分发到多个聊天或频道，bot 需要相应发言权限。列表非空时优先于旧配置 `telegram.target_chat_id`；列表为空时继续使用旧配置，单目标为 `0` 时回复提交私聊。支持 `/help`、`/status ID`、`/retry ID`、`/retry_delivery ID`、`/weekly` 和 `/weekly START END`。

Telegram accepts private messages from users on the allowlist. Acknowledgements, command replies, and processing failure notices stay in private chat. Analysis and digest results default to the submitting chat. Set `telegram.target_chat_ids` to distribute the same result to multiple chats or channels; the bot needs posting permission. A nonempty list takes precedence over the legacy `telegram.target_chat_id`; an empty list uses that legacy setting, with `0` falling back to private chat. Supported commands include `/help`, `/status ID`, `/retry ID`, `/retry_delivery ID`, `/weekly`, and `/weekly START END`.

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
- X 通过公开 oEmbed 读取可用的单条帖子。分析时先读取全部提交入口，再在页面数与深度限制内自动访问 X 正文中提取到的外部链接（包括 `t.co` 短链），将链接网页正文作为独立来源交给模型；访问失败或达到限制时保留说明。自动访问只追读一层，进一步追读由 Agent 按信息缺口决定。长帖、线程回复和链接卡片可能仍不完整。 / X posts are read through public oEmbed. Analysis reads all submitted entries first, then automatically follows external links extracted from the X post text, including `t.co` short links, within page and depth limits. Linked page text is supplied to the model as separate sources; fetch failures and reading limits are retained. Automatic follow-ups cover one level; the agent decides further reads based on information gaps. Long posts, threads, and link cards may still be incomplete.
- 默认单次分析最多计入 6 个页面，其中用于核对事实或补充背景的页面最多 2 个；追读原文也计入页面总数。首次读取页面时，缓存命中和失败尝试也会计数；同一会话再次读取同一页面不重复计数。首个页面深度为 0，默认最多追读到深度 2、迭代 10 次；这些限制可在 `agent` 配置中调整。 / By default, an analysis counts up to 6 pages in total, including primary-source follow-ups, with up to 2 pages for checking facts or adding context. A page's first read counts even on a cache hit or fetch failure; rereading it in the same session does not count again. The first page has depth 0; the default maximum depth is 2, with up to 10 iterations. These limits are configurable under `agent`.
- 周报 `weekly.max_input_chars` 默认 120000；输入超过上限时任务失败并保留完整输入快照，不通过丢弃条目缩小输入。 / `weekly.max_input_chars` defaults to 120000; oversized digest jobs fail with the full input snapshot preserved, rather than dropping entries to fit.
- `weekly.enabled` 默认关闭；启用后按 `weekly.timezone` 和配置的星期、时间调度，默认时区为 `Asia/Shanghai`。 / `weekly.enabled` defaults to false. When enabled, digests follow `weekly.timezone` and the configured weekday and time; the default timezone is `Asia/Shanghai`.
- 网页读取失败后改用浏览器的功能尚未实现；`reader.browser_fallback.enabled: true` 会被拒绝。 / Browser fallback is not implemented; `reader.browser_fallback.enabled: true` is rejected.

## 日志与隐私 / Logs and privacy

启用 `agent.streaming` 后，SSE 输出和工具参数会逐段写入 info 日志；非流式模式也会记录完整模型回复。日志可能包含提交内容或提取文本。SQLite 归档也包含提交和来源正文，请妥善保管本地日志和数据库。

With `agent.streaming` enabled, SSE output and tool arguments are logged incrementally at info level. Non-streaming mode also logs the full model response. Logs may contain submitted text or extracted content. SQLite archives contain submissions and source text; protect local logs and database files.

本地测试和构建不能代替真实外部联调。模型 endpoint、X 页面和 Telegram bot 需要在目标部署环境分别验证。

Local tests and builds do not replace external integration checks. Validate the model endpoint, X pages, and Telegram bot in the target environment.

## 设计 / Design

架构、数据流、可靠性和安全契约见[设计说明](docs/DESIGN.md)。

See the [design document](docs/DESIGN.md) for architecture, data flows, and reliability and security contracts.
