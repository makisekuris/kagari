# `personal` 模块

根包定义个人助手的业务类型、状态、读取投影和共享契约。模型调用、持久化和运行调度分别由子包负责。

| 包 | 职责 |
| --- | --- |
| `personal` | Event、Task、Run、Memory、Schedule 等共享类型，以及跨模块一致的业务规则 |
| `sqlite` | 数据库连接、schema、附件持久化、诊断查询和状态事务 |
| `assistant` | 主对话 Agent、任务/记忆/计划操作工具、对话上下文选择和回复处理 |
| `execution` | 后台任务阶段、工作区、执行上下文和输入处理报告 |
| `agentctx` | 对话与执行器共用的预算控制、记忆注入、材料读取和工具输出限制 |
| `service` | 事件与任务 worker、并发控制、计划调度、恢复和投递 |
| `internal/textclip` | 共享的 Unicode 文本截断规则 |
| `internal/testkit` | 测试中的业务场景准备、模型响应夹具和真实模型配置 |

`cmd/kagari` 创建 Store、MainAgent、TaskExecutor 和 Runtime，并注入回调。实现子包依赖根包的业务契约；根包不依赖实现子包。`assistant` 与 `execution` 共用 `agentctx`，`service` 通过仓库接口和回调运行，不创建模型或数据库连接。

任务、计划、事件、输入交接和消息投递存在跨表状态转换，因此相关 SQL 保持在 `sqlite` 内的同一个事务中。SQL 辅助函数只在存储包内共享；业务类型与模型工具不持有数据库连接。

工具查询遇到缺失记录时使用 `personal.ErrRecordNotFound`，SQL 错误的转换由 `sqlite` 负责；`Task`、`Run` 等可选查询仍以 `nil, nil` 表示不存在。工具记录的调用 ID 由 `agentctx` 从模型消息中提取，`sqlite` 接收正文及元数据，不解析模型框架的消息类型。

执行摘要是完整结果的读取投影。裁剪摘要不修改已保存的结果；较大的进展和输入处理记录通过 `read_progress`、`read_run` 读取，文件路径与附件 ID 保持完整。

改动时按实际职责定位：调整回复决策看 `assistant`，调整阶段执行看 `execution`，调整 worker 生命周期看 `service`，调整原子状态转换看 `sqlite`。共用逻辑放在拥有其规则的包中，调用者复用该实现。

测试随实现归档。存储测试可以检查私有数据库状态和注入 SQL 失败；模型、执行器和调度测试通过业务入口或仓库替身检查状态变化、授权、错误传播和恢复。模型夹具用于触发行为，验收断言检查行为结果；提示词措辞和内部函数拆分不作为独立的业务验收标准。真实模型用例仍需显式启用 `KAGARI_LIVE_EVAL=1`。

运行整个模块的测试使用 `go test ./internal/personal/...`；并发检查使用 `go test -race ./internal/personal/... ./cmd/kagari`。只运行根包会遗漏实现子包的测试。
