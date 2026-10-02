# Kagari

Kagari 是一个 Go 个人阅读助手。它从 CLI 或 Telegram 接收链接，读取网页与公开 X 单帖，生成带来源依据的分析，并保存到本地 SQLite。周报 Agent 可以回顾已保存的分析、合并重复内容并按分类整理。

Kagari is a personal reading assistant written in Go. It accepts links from the CLI or Telegram, reads web pages and public X posts, produces source-backed analysis, and stores it in local SQLite. A weekly digest agent can review saved analyses, merge duplicates, and group them by category.

运行需要 Go 1.27；进程锁使用 `flock`，支持 macOS 和 Linux。模型 endpoint、Telegram bot 和频道权限由部署者配置。

Requires Go 1.27. The process lock uses `flock` and supports macOS and Linux. You provide the model endpoint, Telegram bot, and any channel permissions.

## Quickstart / 快速开始

### 1. 配置 / Configure

复制示例文件。`-n` 会保留已有的本地配置：

Copy the examples. `-n` leaves existing local files unchanged:

```sh
cp -n .env.example .env
cp -n config.example.yaml config.yaml
cp -n profile.example.md profile.md
```

在 `.env` 中填写 `KAGARI_MODEL_BASE_URL`、`KAGARI_MODEL_NAME` 和 `KAGARI_MODEL_API_KEY`。使用 Telegram 时填写 `KAGARI_TELEGRAM_TOKEN`，并在 `config.yaml` 的 `telegram.allowed_user_ids` 中加入允许使用 bot 的用户 ID。

Set `KAGARI_MODEL_BASE_URL`, `KAGARI_MODEL_NAME`, and `KAGARI_MODEL_API_KEY` in `.env`. For Telegram, also set `KAGARI_TELEGRAM_TOKEN` and add permitted user IDs to `telegram.allowed_user_ids` in `config.yaml`.

### 2. 构建并试读 / Build and read

```sh
mkdir -p bin
go build -o bin/kagari ./cmd/kagari
./bin/kagari -config config.yaml read https://example.com/article
```

`read` 不需要模型凭证。配置好模型后，可分析链接：

`read` does not require model credentials. Configure a model endpoint to analyze a link:

```sh
./bin/kagari -config config.yaml analyze -user 123456 https://example.com/article
```

启动 Telegram 服务：

Start the Telegram service:

```sh
./bin/kagari -config config.yaml run
```

## 配置 / Configuration

所有命令都会读取**当前工作目录**的 `.env`，无需手动 `source`。配置优先级为 **系统环境变量 > .env > YAML > 默认值**；显式设置的空值也会覆盖低优先级配置。`-config` 只指定 YAML 路径，不改变 `.env` 查找目录。

Commands load `.env` from the current working directory; no `source` command is needed. Precedence is **process environment > .env > YAML > defaults**. An explicitly set empty value overrides lower-priority values. `-config` selects the YAML file without changing where `.env` is loaded from.

| 设置 / Setting | 默认值 / Default | 说明 / Description |
| --- | --- | --- |
| `storage.path` | `data/kagari.db` | SQLite 数据库 / SQLite database |
| `profile_path` | `profile.md` | 人格和阅读偏好 / Persona and reading preferences |
| `max_attempts` | `6` | 包含首次尝试 / Includes the first attempt |
| `agent.streaming` | `false` | 启用 Responses SSE 与实时日志 / Enables Responses SSE and live logs |
| `weekly.enabled` | `false` | 周报定时调度 / Scheduled weekly digests |
| `weekly.max_input_chars` | `120000` | 周报完整模型输入字符上限 / Maximum serialized digest input size |

`.env`、`config.yaml` 和 `profile.md` 已被 Git 忽略。不要将真实凭证写入示例文件或提交到仓库。

`.env`, `config.yaml`, and `profile.md` are Git-ignored. Keep real credentials out of example files and commits.

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
- `digest [-user ID] [-cutoff RFC3339]` 回顾截止时间前七个日历日；`-start` 与 `-end` 可指定半开日期范围，必须同时提供且不能与 `-cutoff` 混用。 / Reviews the seven calendar days before the cutoff. Use `-start` and `-end` together for a half-open date range; they cannot be combined with `-cutoff`.
- `status ID` 查看任务和投递状态；`export ID` 输出任务 JSON。 / Shows job and delivery status; `export ID` prints the job JSON.
- `retry ID` 重新排队失败任务；`retry-delivery ID` 手动重发失败或结果不确定的投递。 / Requeues a failed job; manually retries a failed or uncertain delivery.

未指定 cutoff 的周报会创建新的时间快照；明确范围的周报可以回放已保存结果。空周报无需模型。默认最大尝试次数为 6，包含首次尝试；中间失败不会向 Telegram 发送最终失败通知。

A digest without a cutoff creates a new time snapshot; an explicit range can replay a saved result. Empty digests need no model call. The default maximum is six attempts, including the first; intermediate failures do not send a final Telegram notice.

CLI 的 `analyze` 和 `digest` 会短暂启动处理器，不能与 `run` 或另一个处理型 CLI 命令同时运行。状态、导出和重试命令可以在服务运行时使用。

CLI `analyze` and `digest` briefly start a processor and must not run alongside `run` or another processing CLI command. Status, export, and retry commands can run while the service is active.

Telegram 第一版只接收私聊中的 allowlisted 用户。默认回复提交所在私聊；配置 `telegram.target_chat_id` 后可将分析结果发往指定 chat 或频道，bot 需要相应发言权限。支持 `/help`、`/status ID`、`/retry ID`、`/retry_delivery ID`、`/weekly` 和 `/weekly START END`。

The first Telegram version accepts allowlisted users in private chats. Replies go to the submitting chat by default. Set `telegram.target_chat_id` to deliver analysis to another chat or channel; the bot needs permission to post there. Supported commands include `/help`, `/status ID`, `/retry ID`, `/retry_delivery ID`, `/weekly`, and `/weekly START END`.

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
- X 通过公开 oEmbed 读取可用的单条帖子；长帖、线程回复和链接卡片可能不完整。 / X posts are read through public oEmbed. Long posts, threads, and link cards may be incomplete.
- 单次分析最多读取 6 页、补读 2 个来源、深度 2、迭代 10 次。周报 `weekly.max_input_chars` 默认 120000；超限任务失败并保留输入快照，不丢弃后续材料。 / One analysis can read up to 6 pages, 2 supplemental sources, depth 2, and 10 iterations. `weekly.max_input_chars` defaults to 120000; oversized jobs fail with their input snapshot preserved.
- `weekly.enabled` 默认关闭；启用后按 `Asia/Shanghai` 时区和配置的星期、时间调度。 / `weekly.enabled` defaults to false. When enabled, digests follow the configured weekday and time in the `Asia/Shanghai` timezone.
- 浏览器回落尚未实现；`reader.browser_fallback.enabled: true` 会被拒绝。 / Browser fallback is not implemented; `reader.browser_fallback.enabled: true` is rejected.

## 日志与隐私 / Logs and privacy

启用 `agent.streaming` 后，SSE 输出和工具参数会逐段写入 info 日志；非流式模式也会记录完整模型回复。日志可能包含提交内容或提取文本。SQLite 归档也包含提交和来源正文，请妥善保管本地日志和数据库。

With `agent.streaming` enabled, SSE output and tool arguments are logged incrementally at info level. Non-streaming mode also logs the full model response. Logs may contain submitted text or extracted content. SQLite archives contain submissions and source text; protect local logs and database files.

本地测试和构建不能代替真实外部联调。模型 endpoint、X 页面和 Telegram bot 需要在目标部署环境分别验证。

Local tests and builds do not replace external integration checks. Validate the model endpoint, X pages, and Telegram bot in the target environment.

## 设计 / Design

架构、数据流、可靠性和安全契约见[设计说明](docs/DESIGN.md)。

See the [design document](docs/DESIGN.md) for architecture, data flows, and reliability and security contracts.
