# MCP 登录与服务交互

更新日期：2026-10-09（Asia/Shanghai）；基于 main `b097886` 的独立 MCP 分支。

Meldra 继续从 `~/.meldra/mcp.json`（或 `$MELDRA_HOME/mcp.json`）读取配置。
OAuth 与静态 `bearer_token_env` 不能同时配置，OAuth 仅适用于 HTTP MCP。

```json
{
  "mcpServers": {
    "remote": {
      "url": "https://example.com/mcp",
      "oauth": {},
      "sampling": true
    }
  }
}
```

`oauth: {}` 使用官方 SDK 的动态客户端注册。若服务提供预注册客户端，配置
`client_id`；需要 secret 时使用 `client_secret_env` 指定环境变量名，并设置
`issuer` 将 secret 绑定到该授权服务器。也可使用 HTTPS 的 `client_id_metadata_url`。
`client_secret_env` 的值不会复制进缓存，刷新时从当前进程环境重新读取；动态注册
由授权服务器签发的 client secret 则保存在私有缓存中，以便后续刷新。
可选 `scopes` 为字符串数组；默认使用服务公布的 scope；可选 `callback_port`
固定本机回调端口（预注册客户端通常需要固定端口），默认使用临时端口。

```sh
meldra mcp login remote
meldra mcp logout remote
```

登录会输出授权 URL，由用户在自己的浏览器打开。回调只监听 `127.0.0.1`，
最长等待五分钟，每个 OAuth HTTP 请求另有 30 秒上限。官方 SDK 执行 PKCE S256、
state、issuer 检查，并在授权和换取 token 时发送 resource 参数；它不代表 Meldra
能够独立验证不透明 access token 的实际 audience。Meldra 拒绝 HTTP 外网授权端点
及重定向。配置目标为 loopback HTTP 时允许
loopback HTTP 授权端点，供本地开发和 E2E 使用。预注册回调路径为 `/callback`。

登录凭据保存在私有目录中的 `mcp-oauth-<server>.json`（0600），按 server URL
及 OAuth 配置绑定。聊天使用已保存的 access token，过期后刷新并保存轮换后的
refresh token；需要重新登录时不在聊天启动期间自动打开浏览器或读取输入。
`logout` 删除本机缓存，不撤销服务端授权；服务端撤销请使用服务本身的账号设置。
即使服务器配置已删除或损坏，仍可用原 server 名称注销残留的本机凭据。
重新登录期间保留原会话；新授权和 MCP 连接都成功后才替换缓存与会话标识。
取消、授权错误或连接失败不会清除原凭据。并发注销或另一登录成功后，较早的
登录尝试不能覆盖当前状态。
授权或 token 交换错误不把敏感请求体输出到终端。
同一 server 的刷新与登录/注销通过跨进程锁串行化；注销或重新登录后，旧会话
不能重新创建缓存。刷新遵守本次 MCP 请求的取消信号。旁边的 `.lock` 和
`.generation` 文件保留用于进程间协调，不包含 access/refresh token。
OAuth 登录、刷新与注销目前仅支持 macOS 和 Linux。

## sampling

仅配置 `sampling: true` 的 server 才获得 sampling 能力。当前支持文本消息，
调用用户已配置的模型；服务的 model preferences 仅为提示，Meldra 使用自己的
模型设置。请求不携带主聊天历史、其他 MCP 内容、workspace 内容或可执行工具。
每次模型请求设置最多 8192 output tokens，且不超过 server 的 `maxTokens`，
模型阶段超时 60 秒；外层 MCP 调用超时也可能更短。结果共享确认使用外层调用
的剩余时间，不继续消耗已经结束的模型阶段超时。
调用模型前及向 server 共享结果前都需要真人确认，`--auto-approve` 不绕过这两次
确认。新请求不复用前一次 sampling 对话。

sampling 已在 MCP `2026-07-28` 标准中标记废弃，目前保留本能力用于旧服务兼容。
不声明 `sampling.tools` 或 `sampling.context`；多模态 sampling 不在当前文本
模型适配器支持范围内。

## elicitation

CLI 和 TUI 均支持 form/URL 请求。form 显示服务器身份、说明和 JSON schema，
用户输入一个 JSON 对象、`decline` 或 `cancel`。TUI 在下方输入框填写，Enter
提交，Esc 取消。字段限扁平字符串、数值、整数、布尔值及字符串枚举数组，最多
32 个字段；输入经 schema 验证，最多允许三次提交（包括首次）。带明显密码、API key、token
等字段名称/描述或敏感格式（如 `password`）的表单会被拒绝；这是额外拦截，不是通用秘密识别器。不要在 form
输入凭据；这类交互必须使用 URL 模式。EOF 取消表单，自动审批不会
伪造答案。form 输入直接交给请求它的 server，server 仍可能在后续结果中回显。
整数输入在 int64/uint64 范围内保持精确，超出范围或带小数部分时要求重新输入。
`number` 字段使用 float64；整数验证不会先转成浮点数。

URL 模式显示完整 URL，不自动打开或预取页面。用户在自己的浏览器完成操作后
确认，或拒绝；第三方页面和输入的凭据不会经过 Meldra 的模型。

Meldra 仅在该 server 的外发 MCP 调用进行中接受交互请求，拒绝空闲期间或其他
server 调用期间主动发起的 sampling/elicitation。新协议的 multi round-trip
和旧协议的服务回调均由官方 SDK 处理。
无人输入时仍按外层调用超时退出；server 单独取消表单后，CLI/TUI 仍可处理
后续交互，不会留下占用输入的读取器或过期审批。

## 验证

```sh
mkdir -p dist
go build -race -o dist/meldra-mcp-race .
python3 scripts/mcp-auth-interaction-e2e.py \
  --binary dist/meldra-mcp-race --output dist/mcp-auth-interaction-repeat
```

输出目录必须不存在。脚本使用本地协议夹具和模型 HTTP 夹具，无真实模型费用。
`report.json`、`checksums.json`、每场景协议、provider 请求和终端记录用于复验；
OAuth 汇总证据记录授权/刷新类型，终端不输出 access token 或 refresh token。
测试使用独立临时配置目录；其他非测试服务或完整 OAuth 一致性需要单独验证。
