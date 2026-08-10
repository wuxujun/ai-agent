# AI Agent 工具列表

> 所有工具通过 `tools.DefaultRegistry` 统一管理，在 `internal/tools/` 各文件的 `init()` 中自注册。  
> **风险等级**：🔴 High（执行前需人工审批）/ 🟢 Low（直接执行）  
> 最后更新：2026-08-10

---

## 工具索引

| # | 工具名 | 风险 | 简述 | 源文件 |
|---|--------|------|------|--------|
| 1 | [`analyze_image`](#1-analyze_image) | 🟢 Low | 分析工作区图片 | `analyze_image.go` |
| 2 | [`apply_patch`](#2-apply_patch) | 🔴 High | 对文件应用 patch | `apply_patch.go` |
| 3 | [`execute_code`](#3-execute_code) | 🔴 High | 在工作区执行命令 | `execute.go` |
| 4 | [`find_files`](#4-find_files) | 🟢 Low | 按 glob 查找文件 | `find.go` |
| 5 | [`git_diff`](#5-git_diff) | 🟢 Low | 查看 git diff | `git_diff.go` |
| 6 | [`http_fetch`](#6-http_fetch) | 🟢 Low | 拉取 HTTP/HTTPS URL 内容 | `http_fetch.go` |
| 7 | [`memory_get`](#7-memory_get) | 🟢 Low | 按 ID 读取历史记忆（Detail） | `retrieval.go` |
| 8 | [`memory_search`](#8-memory_search) | 🟢 Low | 搜索历史任务记忆 | `retrieval.go` |
| 9 | [`rag_fetch`](#9-rag_fetch) | 🟢 Low | 按 ID 读取 RAG 证据（Detail） | `retrieval.go` |
| 10 | [`rag_search`](#10-rag_search) | 🟢 Low | 搜索外部 RAG 知识库 | `retrieval.go` |
| 11 | [`read_file`](#11-read_file) | 🟢 Low | 读取工作区文件内容 | `read.go` |
| 12 | [`search_text`](#12-search_text) | 🟢 Low | ripgrep 正则文本搜索 | `rg.go` |
| 13 | [`sql_query`](#13-sql_query) | 🟢 Low | 只读查询 SQLite 数据库 | `sql_query.go` |
| 14 | [`use_skill`](#14-use_skill) | 🟢 Low | 加载 Skill 指令 | `use_skill.go` |
| 15 | [`web_browser`](#15-web_browser) | 🟢 Low | 无头渲染网页为纯文本 | `web_browser.go` |
| 16 | [`web_search`](#16-web_search) | 🟢 Low | 联网搜索（Firecrawl/DuckDuckGo） | `web_search.go` |
| 17 | [`write_file`](#17-write_file) | 🔴 High | 写入/覆盖工作区文件 | `write.go` |
| 18 | [`json_query`](#18-json_query) | 🟢 Low | 使用 JSON Pointer 只读查询 JSON | `json_query.go` |
| 19 | [`run_tests`](#19-run_tests) | 🔴 High | 受限执行 Go 测试 | `run_tests.go` |

---

## 工具详情

### 1. `analyze_image`

- **风险等级**：🟢 Low
- **描述**：Analyze an image file in the workspace using the configured vision model
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Workspace-relative image path |
  | `prompt` | string | Question or analysis instruction for the image |

- **约束**：
  - 图片必须为工作区内相对路径，不能以 `/` 开头或含 `..`
  - 支持格式：PNG、JPEG、GIF、WebP
  - 文件大小上限：10 MiB
  - 需配置 `LLMSceneVisionAnalyzer` LLM 场景
- **输出**：图片描述、检测文字、对象列表、警告
- **重试策略**：无自动重试

---

### 2. `apply_patch`

- **风险等级**：🔴 **High**（执行前需人工审批）
- **描述**：Apply a patch to a file in the workspace. Supports both SEARCH/REPLACE blocks and Unified Diff formats.
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Workspace-relative path of the file to patch |
  | `patch` | string | Patch content（SEARCH/REPLACE 块 或 Unified Diff 格式） |

- **约束**：
  - `path` 不能为绝对路径或含 `..`
  - `patch` 不能为空
  - 优先尝试 SEARCH/REPLACE 格式；失败后回退到 `git apply` / `patch -p1`
  - SEARCH 段必须在文件中唯一匹配（重复匹配时报错）
- **重试策略**：不自动重试（高风险工具）

---

### 3. `execute_code`

- **风险等级**：🔴 **High**（执行前需人工审批）
- **描述**：Execute a command in the workspace
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `command` | string | Executable to run；必须在 policy 白名单内（e.g. python3, go, bash） |
  | `args` | string | Space-separated arguments passed to the command |

- **约束**：
  - `command` 不能为空
  - `args` 为空格分隔的参数字符串
  - 命令在工作区目录下执行，通过 policy 白名单限制
  - 输出截断至 4000 字符
- **重试策略**：不自动重试（高风险工具）

---

### 4. `find_files`

- **风险等级**：🟢 Low
- **描述**：Find files matching a pattern in the workspace
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `pattern` | string | Filename glob pattern，e.g. `*.go` |

- **约束**：
  - `pattern` 不能为空
  - 底层调用 `find . -type f -name <pattern>`
  - 最多返回 20 个文件
- **重试策略**：最多重试 2 次，间隔 1s

---

### 5. `git_diff`

- **风险等级**：🟢 Low
- **描述**：Get the git diff for the workspace or a specific file
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Optional workspace-relative path；空则 diff 整个工作区 |

- **约束**：
  - `path` 不能以 `/`、`..` 或 `-` 开头（防止 git 参数注入）
  - 通过 `--` 分隔符隔离路径与 git 选项
  - 输出截断至 4000 字符
- **重试策略**：无自动重试

---

### 6. `http_fetch`

- **风险等级**：🟢 Low
- **描述**：Fetch content from an HTTP/HTTPS URL
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `url` | string | Absolute http/https URL to fetch |

- **约束**：
  - URL 必须以 `http://` 或 `https://` 开头
  - 通过 `policy.ValidateURL` 阻止 SSRF（私有地址/回环地址）
  - 响应体读取上限：4000 字节
  - 超时：15 秒
- **重试策略**：最多重试 2 次，间隔 1s

---

### 7. `memory_get`

- **风险等级**：🟢 Low
- **描述**：Read selected historical memory candidates by IDs returned from memory_search
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `ids` | array[string] | Candidate IDs returned by `memory_search`；1~3 个（默认上限） |

- **约束**：
  - `ids` 至少 1 个，最多受 `RAG.JITFetchMaxItems` 配置限制（默认 3）
  - 内容总大小受 `RAG.JITMemoryFetchMaxBytes` 限制（默认 2000 字节）
  - 需要任务执行上下文（`retrievalExecutionContext`）
  - 与 `memory_search` 配套使用，不暴露给 LLM 主动选择（Coordinator 自动插入）
- **重试策略**：无自动重试

---

### 8. `memory_search`

- **风险等级**：🟢 Low
- **描述**：Search local long-term historical memories and return compact candidate IDs; use memory_get to read selected memories
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `query` | string | Focused retrieval query |
  | `top_k` | integer | Maximum candidates to return，1~5 |

- **约束**：
  - `query` 不能为空
  - `top_k` 范围 1~5（超出则使用默认值 5）
  - 返回候选 ID 列表；需配合 `memory_get` 读取完整内容
  - 同一 query 有去重/缓存逻辑，单任务内最多调用 `RAG.JITSearchMaxCalls` 次（默认 2）
- **重试策略**：无自动重试

---

### 9. `rag_fetch`

- **风险等级**：🟢 Low
- **描述**：Read selected current RAG evidence by IDs returned from rag_search
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `ids` | array[string] | Candidate IDs returned by `rag_search`；1~3 个（默认上限） |

- **约束**：
  - `ids` 至少 1 个，最多受 `RAG.JITFetchMaxItems` 配置限制（默认 3）
  - 内容总大小受 `RAG.JITRAGFetchMaxBytes` 限制（默认 6000 字节）
  - 需要任务执行上下文
  - 与 `rag_search` 配套使用，不暴露给 LLM 主动选择
- **重试策略**：无自动重试

---

### 10. `rag_search`

- **风险等级**：🟢 Low
- **描述**：Search current third-party RAG knowledge and return compact candidate IDs; use rag_fetch to read selected evidence
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `query` | string | Focused retrieval query |
  | `top_k` | integer | Maximum candidates to return，1~5 |

- **约束**：
  - `query` 不能为空
  - `top_k` 范围 1~5
  - 需配置 `RAG.SearchURL`（第三方 RAG 服务）或本地 Store
  - 返回候选 ID 列表；需配合 `rag_fetch` 读取完整证据
  - 同一 query/cycle 有去重与 cycle 上限（`RAG.JITRetrievalMaxCycles`，默认 2）
- **重试策略**：无自动重试

---

### 11. `read_file`

- **风险等级**：🟢 Low
- **描述**：Read the contents of a file in the workspace
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Workspace-relative file path to read |

- **约束**：
  - `path` 不能为空、不能以 `/` 开头或含 `..`
  - 通过 `policy.ValidateReadPath` 限制在工作区边界内
  - 文件内容截断至 4000 字符
- **重试策略**：无自动重试

---

### 12. `search_text`

- **风险等级**：🟢 Low
- **描述**：Search for text matching a regex query in the workspace
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `query` | string | Regex pattern to search for |
  | `glob` | string | Optional file glob to restrict the search，e.g. `*.go` |

- **约束**：
  - `query` 不能为空
  - `glob` 长度不超过 100 字符
  - 底层使用 `rg`（ripgrep），返回最多 8 条 Evidence
  - 输出格式：`file:line:content`
- **重试策略**：最多重试 2 次，间隔 1s

---

### 13. `sql_query`

- **风险等级**：🟢 Low
- **描述**：Execute a read-only SQL query against a SQLite database in the workspace. Protected by read-only AST checks.
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Workspace-relative path to SQLite database file（默认 `data/agent.db`） |
  | `query` | string | SQL SELECT query to execute |

- **约束**：
  - `query` 不能为空
  - `path` 不能为绝对路径或含 `..`
  - 通过 AST 检查只允许只读 SQL（SELECT 等）；INSERT/UPDATE/DELETE/DROP 等被拒绝
  - 通过 `policy.ValidateReadPath` 限制在工作区内
- **重试策略**：最多重试 2 次，间隔 1s

---

### 14. `use_skill`

- **风险等级**：🟢 Low
- **描述**：Load a skill's full instructions by name before performing a specialized task. Choose a name from the listed available skills, then follow the returned instructions using the other tools.
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `name` | string | Skill name to load；必须完全匹配已注册的 skill 名称 |

- **约束**：
  - `name` 不能为空
  - 仅返回 SKILL.md 指令内容，不执行任何副作用（只读操作）
  - 通过 `RegisterUseSkill(skillReg)` 在 `main()` 中显式注册（非 `init()` 自动注册）
  - Skill 实际的写操作由后续调用 `write_file`/`execute_code` 等完成，仍走各自的审批流
- **重试策略**：无自动重试

---

### 15. `web_browser`

- **风险等级**：🟢 Low
- **描述**：Load and render a webpage as readable text, stripping scripts, styles, and HTML tags.
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `url` | string | Absolute URL of the webpage to load（必须以 http:// 或 https:// 开头） |

- **约束**：
  - URL 必须以 `http://` 或 `https://` 开头
  - 通过 `policy.ValidateURL` 防止 SSRF
  - 响应体读取上限：1 MB
  - 自动剥离 `<script>`、`<style>`、`<head>`、`<noscript>`、`<iframe>` 标签
  - 超时：15 秒
- **重试策略**：最多重试 2 次，间隔 1s

---

### 16. `web_search`

- **风险等级**：🟢 Low
- **描述**：Search the web using the configured search provider (e.g. Firecrawl)
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `query` | string | Search keywords |

- **约束**：
  - `query` 不能为空
  - 默认使用 Firecrawl（POST `/v1/search`），返回最多 5 条结果
  - 也支持 DuckDuckGo 等 GET 模板 URL（通过 `config.Search.URL` 配置）
  - 通过 `policy.ValidateURL` 防止 SSRF
  - 响应体读取上限：1 MB
  - 超时：15 秒
- **重试策略**：最多重试 2 次，间隔 1s

---

### 17. `write_file`

- **风险等级**：🔴 **High**（执行前需人工审批）
- **描述**：Write content to a file in the workspace
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Workspace-relative file path to write |
  | `content` | string | Content to write to the file |

- **约束**：
  - `path` 不能为空、不能以 `/` 开头或含 `..`
  - 通过 `policy.ValidateWritePath` 限制在工作区边界内
  - 自动创建父目录（`MkdirAll`）
  - 使用 `O_NOFOLLOW` 打开文件，防止符号链接攻击
  - 全量覆盖写入（`O_TRUNC`）
- **重试策略**：不自动重试（高风险工具）

---

### 18. `json_query`

- **风险等级**：🟢 Low
- **描述**：读取 Workspace JSON 文件并使用 RFC 6901 JSON Pointer 选取结构化值。
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `path` | string | Workspace 相对 JSON 文件路径 |
  | `pointer` | string | JSON Pointer，例如 `/users/0/name`；空值选取根节点 |

- **约束**：
  - 禁止绝对路径和 `..`，通过 Workspace 边界校验并使用 `O_NOFOLLOW`
  - 输入文件最大 2 MiB
  - 支持对象键、数组下标和 `~0` / `~1` 转义
  - 拒绝非法 Pointer、越界下标和多个顶层 JSON 值
- **重试策略**：无自动重试

---

### 19. `run_tests`

- **风险等级**：🔴 **High**（执行前需人工审批）
- **描述**：在 Workspace 中执行受限 Go 测试。
- **参数**：

  | 参数 | 类型 | 说明 |
  |------|------|------|
  | `package` | string | `./...` 或 `./internal/store` 等 Workspace 相对 package |
  | `run` | string | 可选 `go test -run` 正则 |
  | `race` | boolean | 是否启用 race detector |

- **约束**：
  - executable 固定为 `go`，子命令固定为 `test`
  - 不接受 shell 字符串或任意附加参数
  - package 拒绝绝对路径、`..` 和空白注入
  - 测试失败作为 Observation 返回；超时/取消作为工具错误
- **重试策略**：不自动重试（高风险工具）

---

## 附：各 Mode 工具覆盖范围

| Mode | 可用工具 | 说明 |
|------|---------|------|
| `eino`（默认） | 全部 19 个 | Planner schema 动态派生自 DefaultRegistry |
| `legacy` | 全部 19 个 | 与 eino 相同，原始实现 |
| `adk` | 全部 19 个 | 从 DefaultRegistry 动态构建 ADK 工具，High Risk 工具要求确认 |
| `step` | 3 个：`find_files`、`search_text`、`read_file` | 硬编码静态 3 步序列，无 LLM |
| `multiagent` | 17 个（Planner 动态枚举，排除 `rag_fetch`/`memory_get`）；Researcher 动态查 Registry | Plan→Research→Write 多智能体 |

> `rag_fetch` 和 `memory_get` 不在 Planner 枚举中，由 Coordinator/Engine 在检测到 `rag_search`/`memory_search` 步骤后自动插入。
