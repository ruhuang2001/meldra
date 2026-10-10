# MCP 调研与验证分支

更新日期：2026-10-09（Asia/Shanghai）。独立分支 `codex/mcp-support` 当前以
`main` 提交 `b097886` 为基线；原工作区的未提交修改未并入本分支。

Meldra 可以连接外部 MCP server，也能通过 `meldra mcp serve` 将自身工作区工具
提供给其他 MCP client。协议复用官方 Go SDK v1.8.0；工具执行、审批、任务记录
和恢复沿用 main 的 runtime。

| 能力 | 当前接入方式与范围 |
| --- | --- |
| tools | stdio、Streamable HTTP；发现、分页、参数验证、调用及审批 |
| resources | 会话中的资源目录工具支持 list、templates、read，保留文本和二进制 JSON 内容 |
| prompts | 会话中的提示目录工具支持 list/get 和参数；返回内容作为不可信工具结果 |
| OAuth | 显式 login/logout；动态/预注册/CIMD 客户端、凭据缓存和刷新 |
| sampling | 按 server 配置启用的文本 sampling；独立模型请求、token 上限、请求和结果共享审批 |
| elicitation | CLI/TUI JSON 表单与 URL 交互；校验、拒绝、取消；支持现代 MRTR 和旧回调 |
| MCP server | stdio；现有 workspace 工具、资源及 review prompt；修改通过客户端 elicitation 审批 |

这些是实际接入的能力范围，并非“最新 MCP 的所有功能”。SDK 支持的协议版本
和 Meldra 的产品功能是两个层面；sampling 已在 `2026-07-28` 规范中标记废弃，
本分支保留其文本兼容能力。

## 参考实现与选择

| 一手资料 | 观察到的实现 | 本分支的选择 |
| --- | --- | --- |
| [官方 Go SDK v1.8.0](https://github.com/modelcontextprotocol/go-sdk/tree/v1.8.0) | Client/ClientSession、协议协商、stdio/Streamable HTTP、分页及调用 | 使用 SDK 处理协议；不自行实现 JSON-RPC。SDK 要求 Go 1.25，Meldra 的 Go 1.26.9 满足要求。 |
| [OpenCode catalog.ts](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/mcp/catalog.ts) | 分页、调用超时/取消、isError 和 structuredContent 转换 | 保留完整结果 JSON，单独处理远端业务失败和传输中断；对目录数量与大小设限。 |
| [OpenCode index.ts](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/mcp/index.ts) | 连接/发现失败关闭 transport，退出时清理后代进程 | 失败的 server 显示 warning，其他工具继续工作；所有退出路径共用连接清理。 |
| [Codex stdio launcher](https://github.com/openai/codex/blob/b741e480e203f037ca726bc2a76d99a8e8668e66/codex-rs/rmcp-client/src/stdio_server_launcher.rs) | Unix 进程组终止；Windows Job 管理 | 本分支按 Meldra 的 macOS/Linux 支持范围创建独立进程组，退出时终止该组。 |
| [Codex connection manager](https://github.com/openai/codex/blob/b741e480e203f037ca726bc2a76d99a8e8668e66/codex-rs/codex-mcp/src/connection_manager.rs) | 启动与调用超时分开；关闭不依赖已取消的请求继续运行 | 默认启动/发现 10 秒，调用 60 秒；清理独立于已取消的调用 context。 |
| [Codex HTTP redirect policy](https://github.com/openai/codex/blob/b741e480e203f037ca726bc2a76d99a8e8668e66/codex-rs/rmcp-client/src/http_client_redirect.rs) | 限制跨源重定向，保护认证和请求体 | 当前实现禁止全部 HTTP 重定向。 |
| [Claude Code MCP trust](https://code.claude.com/docs/en/mcp#project-server-approvals-and-workspace-trust) | 区分项目配置与信任/批准 | 只读取私有用户目录中的 mcp.json；不自动启动仓库提交的 MCP 配置。 |

[MCP tools 规范](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)
是工具定义、调用结果和错误语义依据。固定 SDK 版本负责协商新旧协议，
包括 `server/discover` 和旧 `initialize` 回退。stdio 服务若在现代探测时退出，
重新启动进程后尝试旧握手；不在工具已经派发后自动重试。测试夹具覆盖部分现代
及旧协议路径，不能据此宣称完整 MCP 规范认证或所有第三方服务兼容。

## 接入结构

`newChatRuntime → connectMCP → ToolDefinition → Agent.runInference / tool.Registry
→ taskExecution.invoke → Workspace.requestApproval → ClientSession`。

- `mcp.go` 负责配置、连接、工具发现和关闭；`mcp_catalog.go` 提供资源和提示目录
  操作；`mcp_oauth.go`、`mcp_interaction.go` 分别处理认证和服务交互。
- CLI、TUI、task resume 共用 runtime；其拥有者关闭连接。
- 外部工具名通常为 `mcp__SERVER__TOOL`；资源和提示目录名通常为
  `mcp_resources__SERVER`、`mcp_prompts__SERVER`。超过模型名称限制时使用摘要。
- 外部 schema 保留原义并设置 `strict:false`，内置工具仍为 strict；工具调用前
  验证参数，不获取外部 `$ref`。资源/提示操作也验证所需字段及大小。
- 外部 tools、resources、prompts 请求走现有审批；`readOnlyHint` 不免除审批。
  `--auto-approve` 可以批准这些请求，但不能自动批准 sampling 或伪造表单回答。
- `isError` 记录为 failed 并反馈模型。工具派发后的断线、超时或取消保守记录为
  unknown，停止模型循环，不自动重复副作用；使用 `task show/resolve/resume`
  处理。main 的 `replay_scope` 防止把不同响应中的相同 call ID 错误去重。
- 完整结果 JSON（含 `structuredContent`、媒体或 binary 内容）写入 artifact，
  最多 16 MiB；传给模型的文本最多 256 KiB。保留媒体不等于新增多模态模型输入。

## 配置与运行

先执行 `meldra config init`，再手动创建 `~/.meldra/mcp.json`；设置
`MELDRA_HOME` 后改用 `$MELDRA_HOME/mcp.json`。目录须为 0700，文件须为 0600，
不能是符号链接。缺少该文件时不启动 MCP server。

```json
{
  "mcpServers": {
    "local": {
      "command": "node",
      "args": ["/absolute/path/to/server.js"],
      "env_vars": ["MY_SERVICE_TOKEN"],
      "env": {"LOG_LEVEL": "warn"},
      "startup_timeout_sec": 10,
      "tool_timeout_sec": 60
    },
    "remote": {
      "url": "https://mcp.example.com/mcp",
      "oauth": {},
      "sampling": false,
      "disabled": true
    }
  }
}
```

```sh
chmod 700 ~/.meldra
chmod 600 ~/.meldra/mcp.json
meldra --workspace /path/to/project
```

每个 server 必须指定 `command` 或 `url` 二者之一。stdio 不经过 shell，工作目录
为 workspace；默认仅继承 PATH、HOME、USER、LOGNAME、临时目录和系统目录变量。
用 `env_vars` 显式继承其他环境变量，`env` 显式覆盖；OPENAI_API_KEY 不自动传递。

HTTP 使用 HTTPS 或 loopback HTTP，拒绝 URL 内凭据/query/fragment 和重定向。
静态认证配置 `bearer_token_env`；浏览器授权配置 `oauth`，两者互斥。OAuth server
先运行 `meldra mcp login SERVER`，之后聊天读取缓存；详细配置和用户交互见
[OAuth、sampling 与 elicitation](mcp-auth-interactions.md)。

启动/发现默认 10 秒，单次 MCP 调用默认 60 秒，可分别配置 1–300 秒。OAuth 登录
单独等待最多五分钟；OAuth metadata/token 请求最多 30 秒，MCP 请求遵循配置的
调用超时。配置最大 1 MiB、
最多 16 个 server；每个工具目录最多 128 个工具、1 MiB schema/description。
stdio 单帧和 HTTP 响应限制为 16 MiB。

**配置授予启动信任**：stdio 进程在 runtime 初始化时启动，早于调用审批，使用
当前 OS 用户权限，可能访问 workspace 之外内容。环境过滤、进程组管理不是沙箱。
只配置可信 server，不在 args/URL 放认证值。参数、工具结果可能进入私有任务历史
和 artifact；这些记录不是加密秘密存储，服务回显的敏感内容也不会自动脱敏。

macOS/Linux 关闭连接时终止其进程组并回收主进程。主动脱离进程组的服务仍可能
存活；SIGKILL、断电不保证清理。server 不应依赖退出回调保存已确认完成的操作。

## resources 和 prompts 的使用

连接成功后，可在聊天中要求列出某个 server 的资源或 prompt，随后指定 URI 或
prompt 名称与参数。资源目录工具的 `action` 为 `list/templates/read`，提示目录
工具为 `list/get`；list 返回一页，并用 `cursor` 继续获取下一页。

资源模板返回 URI 模板，实际读取使用展开后的 URI；二进制内容按 MCP JSON 编码
保留。prompt 返回的消息保留角色及内容，但不会被静默提升为系统指令。远端内容
始终是不可信数据。

也可以直接选择服务，无需配置模型密钥即可查看结果：

```sh
meldra mcp resources SERVER [CURSOR]
meldra mcp templates SERVER [CURSOR]
meldra mcp read SERVER URI
meldra mcp prompts SERVER [CURSOR]
meldra mcp prompt SERVER NAME KEY=VALUE
meldra mcp prompt SERVER NAME KEY=VALUE --run
```

这些命令只连接指定服务器，可指定 `--workspace PATH`。`prompt` 默认输出完整
JSON；`--run` 展示文本提示并确认后启动模型工作流，角色只作为用户输入中的标签，
不会变成系统权限。非文本 prompt 仍可查看，但不能交给当前文本工作流运行。
资源订阅、目录变更通知和 prompt 参数自动补全尚未接入。

## 将 Meldra 作为 MCP server

```sh
meldra mcp serve --workspace /path/to/project
```

由上游 MCP client 启动此命令，stdin/stdout 专用于 JSON-RPC；不打开聊天 TUI，
也不需要模型 API key。server 暴露现有 workspace 工具，并提供：

- `meldra://workspace`：有界工作区文件列表。
- `meldra://workspace/{path}`：有界文件内容，路径按 URI 编码。
- `review_workspace` prompt：可选字符串参数 `focus`。

读写仍遵循 workspace 路径、`.git`、Meldra 配置目录保护及命令 allowlist；执行
记录进入现有任务存储。需要批准的操作经上游 client 的 form elicitation 请求
真人确认；client 不支持或拒绝交互时不执行。现代协议的批准状态绑定具体操作
及参数且有有效期；旧协议使用服务回调。

`--auto-approve` 显式跳过修改审批，仅用于已授权的隔离环境。这个模式目前只提供
stdio，没有监听 HTTP 端口、后台 daemon 或将其他 MCP server 转发出去。

## 可重复验证

[失败矩阵](mcp-e2e-failures.md) 记录原始 tools 测试设计。新增认证/交互脚本也在
文件顶部记录失败范围。测试驱动真实 Meldra 二进制，MCP 和模型端使用本地协议
夹具，不使用真实 API key、不产生模型费用。

```sh
make check
mkdir -p dist
go build -race -o dist/meldra-mcp-race .
python3 scripts/mcp-e2e.py \
  --binary dist/meldra-mcp-race --output dist/mcp-e2e-repeat
python3 scripts/mcp-auth-interaction-e2e.py \
  --binary dist/meldra-mcp-race --output dist/mcp-interaction-repeat
python3 scripts/mcp-cli-e2e.py \
  --binary dist/meldra-mcp-race --output dist/mcp-cli-repeat
python3 scripts/mcp-input-e2e.py \
  --binary dist/meldra-mcp-race --output dist/mcp-input-repeat
python3 scripts/mcp-oauth-regression-e2e.py \
  --binary dist/meldra-mcp-race --output dist/mcp-oauth-repeat
```

`make check` 同时运行以上五组 E2E 和 Go race tests，以 Go 的二进制覆盖率工具
合并真实执行记录，并继续执行原有 75% 门槛；不会用删减测试或降低门槛代替验证。

输出目录必须不存在，避免覆盖证据。脚本依赖 macOS/Linux、Python 3 标准库和
本地回环端口权限；PTY 场景验证真实 TUI。结果写入 `report.json` 和
`checksums.json`，各场景保留协议、provider 请求、终端及任务 artifact。

`dist/mcp-e2e-final` 等早期目录属于 tools 阶段的历史证据；后续交互验证记录在
`dist/mcp-interaction-final` 等目录。历史结果不代表之后修改自动通过。复核当前
版本应重建二进制并执行以上命令，读取本次产生的报告，不沿用旧测试数量或覆盖率。

这些检查证明所覆盖的本地集成路径，不能代替真实 OAuth 提供方、真实模型选择工具
行为、所有第三方 MCP server 或完整规范的验收。证据包含本机路径及测试历史，
留在忽略的 `dist`；测试脚本进入分支。

## 明确边界

暂未接入资源订阅、动态工具目录通知、completion 自动补全、HTTP server 模式、
sampling 多模态/tools/context、项目级 MCP 配置和自动重连。配置改变需重启聊天。
SDK 对新旧协议的支持不等于这些产品能力已经实现。
