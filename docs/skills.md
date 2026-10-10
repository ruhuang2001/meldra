# Meldra Skills 调研与验证原型

整理日期：2026-10-04（Asia/Shanghai）；资料检索始于 2026-10-03。
验证分支：codex/skills-support，基于提交 4e75ff7，使用独立工作树。

## 结论

Meldra 可以把 Skills 实现为“可发现的工作流说明 + 按需读取的资源”，复用现有执行器。
原型已经接通发现、离线列表、模型目录、read_skill、工具审批和任务审计。
脚本仍走现有工作区命令工具，不需要另建运行时。

本次验证的是应用链路，并未宣称完整兼容其他 agent 的权限扩展、插件安装或斜杠命令。
$name 当前是提示词约定，由模型调用读取工具，不是前端强制加载。

## 参考实现

| 参考 | 发现与重名 | 加载与调用 | 对 Meldra 的启发 |
| --- | --- | --- | --- |
| Agent Skills 规范 [1] | 定义包格式，不规定目录和优先级 | YAML 元数据、Markdown 正文、可选脚本与资源；集成指南建议渐进式加载 | 兼容格式和分层加载；发现策略由 Meldra 明确定义 |
| Codex 官方文档 [2] | 仓库各层 .agents/skills、用户及管理/系统目录；同名技能可以同时显示 | 初始给名称、描述和路径；使用时读取正文；显式提及与隐式匹配 | 目录进入上下文，正文和参考文件按需读取 |
| Claude Code 官方文档 [3] | .claude/skills，含项目、个人、企业等来源；当前文档对同名目录规定企业 > 个人 > 项目 | /skill-name、参数替换、调用控制字段；allowed-tools 可临时授予免审批权限 | 不把扩展字段当作通用规范，不自动转换为 Meldra 授权 |
| OpenCode 官方文档与源码 [4] | 原生目录及 .claude/.agents 兼容目录；递归扫描并支持附加来源 | 专用 skill 工具返回正文与基目录，受 skill 权限控制；技能命令避让已有命令 | 专用读取工具便于审计，但发现和重名处理必须明确 |

OpenCode 源码固定在提交 907b3bc518fa48e90e8ec24dd327d13eee71c36c。
检查了 skill/index.ts、skill/discovery.ts、tool/skill.ts 和 command/index.ts。
加载采用并发 Effect.forEach，同名条目在警告后覆盖映射；不能据此推导稳定的“最后扫描者优先”。
专用工具调用 ctx.ask；命令注册通过已有命令检查避让冲突。

应分别评估格式、发现和运行策略。集成指南 [1] 对“项目覆盖用户”的概括与当前
Claude Code 文档并不一致；Meldra 不把自己的优先级描述为通用兼容规则。

## 原型规则

按以下顺序扫描每个目录直接子级的 <name>/SKILL.md：

1. 工作区 .meldra/skills。
2. 工作区 .agents/skills。
3. $MELDRA_HOME/skills，默认 ~/.meldra/skills。
4. $HOME/.agents/skills。

同名采用第一个有效且被收录的包，并警告被遮蔽来源；最终目录按名称排序。
缺失目录跳过，单个损坏包不阻止其他包使用。只扫描当前工作区，不向父仓库查找，
不递归搜索，也不读取 .claude、.codex 等私有来源。

SKILL.md 必须为 UTF-8 文本，开头包含 YAML frontmatter：

- name：1–64 个 ASCII 小写字母、数字或单连字符；不能有首尾/连续连字符，必须与目录名一致。
- description：非空字符串，最多 1024 个 Unicode 字符。
- 支持引号、折叠文本和 CRLF；重复键及字段类型错误被拒绝。
- 其他字段不参与策略。allowed-tools、disable-model-invocation、context、参数替换和 agents/openai.yaml 均未实现；不能依赖这些字段获得权限或禁止隐式调用。

Go 标准库及项目原有依赖没有 YAML 解析器。新增 go.yaml.in/yaml/v3 v3.0.5，
避免手写无法正确处理常见 frontmatter 的解析器。

| 限制 | 行为 |
| --- | --- |
| 单文件 128 KiB | 说明或资源超限均拒绝，不将截断正文当作完整说明 |
| YAML 头部 16 KiB | 超限或缺少结束分隔符则拒绝 |
| 合计 256 个直接目录条目 | 当前根目录超出剩余额度时，该根及后续来源全部跳过并警告，避免随机选取 |
| 初始目录 JSON 64 KiB | 无法放入的条目被跳过并警告；正文不进入初始目录 |

目录是启动时快照，重启或恢复会话时重新发现。每次 read_skill 都读取当前文件；
修改正文不要求重建目录，新增、重命名或修改描述需要重启/恢复。

## 接入点与边界

newChatRuntime 在设置工作区保护路径后发现技能，有可用技能时注册 read_skill。
runInference 向每次模型请求追加名称、描述和路径，不预先注入正文。
模型可以根据任务匹配描述，或响应用户的 $name 提及。

工具参数示例：

    {"name":"review-checklist","path":null}

path=null 读取 SKILL.md；path=references/checklist.md 读取该技能目录内的文本资源。
结果包含名称、基目录和资源路径。成功读取复用任务系统的摘要寻址 artifact，
工具作用类型为 read。恢复使用原有任务、会话和审计机制，无新增存储表。

配置、任务数据和执行锁保护对所有来源生效，包括工作区外的用户包。
唯一配置目录例外是 read_skill 可读取 $MELDRA_HOME/skills 内的包；普通 read_file
仍不能访问配置目录。拒绝绝对路径、..、.git、符号链接、非普通文件、二进制和超限资源。
目录逐级用 os.Root 打开并验证身份；macOS/Linux 使用 O_NOFOLLOW 和 O_NONBLOCK，
防止最终文件被替换为符号链接或 FIFO 后跟随/阻塞。

技能内容不能更改用户意图或运行时权限。read_skill 返回的说明仅作为从属工作流指导；
其他文件与命令输出仍是不可信数据。读取不会运行脚本、授予写权限或添加工具。
工作区脚本仍走现有命令白名单和审批；外部包脚本需经审批复制到工作区才能执行。
已批准的命令仍以用户身份运行，Skills 没有增加操作系统沙箱。

压缩不保证完整保留已读取正文；提示词要求需要时重新读取。本次验证跨进程恢复
重新发现和读取，未证明真实模型在压缩后主动重读的可靠性。

## 试用

在测试项目建立 .agents/skills/review-checklist/SKILL.md：

    ---
    name: review-checklist
    description: 在用户要求代码审查时检查正确性、错误处理和验证证据。
    ---
    读取改动及其调用点。优先报告可复现问题，并给出文件位置。
    使用现有工具验证；执行命令前遵守 Meldra 审批。

在独立工作树中构建，并指向测试项目：

    mkdir -p dist && go build -o dist/meldra-skills .
    ./dist/meldra-skills skills --workspace /path/to/test-project --json
    ./dist/meldra-skills --workspace /path/to/test-project --prompt '使用 $review-checklist 审查当前改动'

列表不需要 API 凭据；聊天按原有方式配置模型和凭据。

## 验证与产物

先写失败清单 [5] 和端到端脚本，再实现功能；未新增单元测试。
scripts/skills-e2e.py 构建真实二进制，隔离 HOME、MELDRA_HOME 和工作区，
通过本地确定性 Responses SSE 服务驱动 CLI、工具、审批、SQLite 记录及会话恢复。
使用虚假凭据，不请求付费模型。

    python3 scripts/skills-e2e.py --output dist/skills-e2e-new-run
    make check

输出目录须不存在或为空；每次使用新目录保留独立证据。
成功产物 dist/pr42-merge-review/skills-e2e/assertions.json 记录 121 项检查通过，保存源码和二进制
SHA-256、HTTP 请求/响应、stdout/stderr、任务/事件 JSON、fixture、实际生成文件及审计 artifact。
dist/pr42-merge-review/check-local-ci.log 记录 make check 通过：race、vet、格式、依赖、构建及 81.2% 覆盖率。
Linux amd64 交叉编译也通过，但未在 Linux 上执行该二进制。

审查发现“工作区外用户技能包含配置目录”可能绕过保护。新增同一黑盒用例，
在保存的旧二进制上复现凭据哨兵泄漏，在修复版上通过。
失败证据在 dist/pr42-review-regression，修复后成功证据在 dist/pr42-review-final。
新增覆盖还包括全局包读取、CLI/运行时缓存保护一致、FIFO 拒绝、审批先拒绝后同意及恢复读取新正文。

这些检查证明协议接入和应用行为；脚本服务替代模型决策，不能声称真实模型已正确自动选择
技能或遵循全部说明。任意并发文件系统替换、硬链接/挂载及恶意指令抵抗不在本 E2E 证明范围内。

## 后续取舍

需要确定性选择时再增加前端加载/选择器；需要只允许显式调用时增加可验证的策略过滤，
不能只靠提示词。目录规模或更新频率确有需求后再考虑分页、热重载和祖先目录发现。
插件市场、安装器、远程下载、新执行器及新权限系统不属于本原型。

## 一手来源

1. Agent Skills：[格式规范](https://agentskills.io/specification)、[集成指南](https://agentskills.io/integrate-skills)。
2. OpenAI 官方文档：[Codex Skills](https://developers.openai.com/codex/skills/)，检索时标题为 Build skills；[Markdown](https://developers.openai.com/codex/skills.md)。
3. Claude Code：[Skills](https://code.claude.com/docs/en/skills)、[同名规则](https://code.claude.com/docs/en/skills#resolve-skills-that-share-a-name)。
4. OpenCode：[官方说明](https://opencode.ai/docs/skills/)、固定提交的 [loader](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/skill/index.ts)、[discovery](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/skill/discovery.ts)、[skill tool](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/tool/skill.ts)、[commands](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/command/index.ts)。
5. 本仓库：[失败清单](skills-failure-matrix.md)、[端到端脚本](../scripts/skills-e2e.py)、[实现](../internal/app/skills.go)。
