# Meldra 0.2.0：前台任务执行范围

范围更新：2026-10-02。0.2.0 聚焦 M1—M4：可恢复、可追踪的前台任务执行。
benchmark、Agent 能力评估及真实模型质量评估不属于本版交付，也不是稳定发布门槛；
原 M5/M6 的评估工程和独立预发布流程已移出范围。
常规正确性、race、覆盖率、平台兼容性和已有发布流程保留。
本文件描述功能和验收约定，不以历史测试成绩宣告当前候选通过或版本已经发布。

## 产品约定

- 任务记录用户目标、每次 Run、工具结果、审批和事件，支持无需模型凭据的只读查询。
- 关闭终端结束当前执行；下一次启动不自动继续，只有显式 resume 才恢复。
- 已确认的工具结果不重放；无法确定的副作用必须核对并显式处理。
- 不引入常驻服务、detach/attach、自动 worktree 管理器、多代理或远程调度。
- Task completed 表示一次执行已结束，不是修改正确或用户目标已验证的证明。
- SQLite 不能与外部命令原子提交，不承诺任意命令恰好执行一次。

## M1：数据契约与所有权

Task、Run、ToolCall、Approval、Event 契约位于 `internal/task`；
`internal/store` 用 SQLite WAL/FULL sync、事务和版本化 schema 保存执行历史。
终态 Run 不被后续恢复覆盖，resume 创建新 Run。私有目录、日志及产物具有大小限制。
任务和可写工作区通过跨进程锁独占，文件系统身份核对覆盖路径别名、独立配置目录
和目录替换。锁退出即释放，不仅依赖 PID 或进程内 mutex。

验收：状态转换、事务中断、并发竞争、迁移重入、损坏记录及未知 schema；
Darwin/Linux 的 amd64/arm64 保持 CGO_ENABLED=0 构建和本机运行检查。

## M2：接入真实执行链路

`internal/app` 的任务适配器连接模型、工具、会话和持久化。工具提供结构化状态、
退出码、耗时、截断和产物引用，模型仍接收兼容文本。副作用前保存意图，执行后
保存结果；落盘失败停止后续副作用。审批先记录再执行，并绑定具体操作及文件指纹。
模型请求、工具和最终答复关联到同一事件链；运行配置不存 API key 或原始 reasoning。

验收：真实读写、patch、命令、拒绝审批、超时、输出限制和存储失败，
以及 CLI/TUI 使用同一执行结果。

## M3：退出与显式恢复

Ctrl-C、SIGTERM、SIGHUP 和 UI 退出取消在途请求及所属命令组，并在有界时间内
保存可确认状态。`--prompt` 后的普通 EOF 允许当前请求完成。
显式恢复先核对遗留调用，再启动新 Run；文件 before/after 指纹不一致时停止覆盖，
未知命令结果要求 `task resolve`。审批不跨操作或 Run 复用。
恢复上下文保留用户目标、持久化请求、关键工具结果及验证信息。

验收：意图前、意图后执行前、执行后结果前、结果保存后及审批边界的真实进程退出；
已知结果不重复执行，用户改动被保留，进程组取消和 PTY 关闭可复现。
SIGKILL、断电及逃离进程组的后代可能阻止正常清理；恢复必须如实保留不确定性。

## M4：用户入口与旧会话兼容

```sh
meldra tasks
meldra task show TASK_ID
meldra task events TASK_ID --json --after 0
meldra task resume TASK_ID
meldra task resolve TASK_ID CALL_ID --outcome succeeded --reason "verified effects"
meldra sessions
meldra resume SESSION_ID
```

只读查询不启动模型或工具。事件 JSONL 包含 schema、稳定 ID 和单调序号，支持分页。
旧 JSON 会话重复导入不丢原始文件，不推测缺失工具记录；当前快照与旧快照分开保存。
`resume latest` 遵循工作区约束。降级只能读取保留的旧快照，不能撤销仓库改动或
读取新任务历史。备份、升级和降级方式见 [README](../README.md#upgrade-and-downgrade)。

验收：重复及中断导入、损坏/缺失快照、工作区移动或丢失、事件分页和命令帮助。

## 完成条件与验证产物

- `make check` 通过：格式、vet、依赖一致性、race、总覆盖率至少 75% 和构建。
- M1—M4 的恢复、审批、存储及兼容性测试通过，无跳过关键行为或未知副作用误重放。
- 已有 CI 和平台兼容性检查通过，README/存储契约与实际命令一致。
- 版本、manifest、tag 和 CHANGELOG 通过已有发布流程推进，评估成绩不作为依赖。

使用现有测试保存可复查的 JSONL 产物（无需真实模型）：

```sh
mkdir -p dist
go test -race -json -count=3 -timeout=5m ./... > dist/m1-m4-tests.jsonl
```

产物保留每个测试的 pass/fail/skip 和输出，命令非零退出即失败。它是行为回归记录，
不是 Agent 能力分数。日志和临时测试产物不提交 Git。

长期文档随实现提交：[架构](architecture.md)、[存储契约](task-storage.md)、
使用和升级说明及本范围约定；历史跑分和逐次验证流水记录不单独维护。
