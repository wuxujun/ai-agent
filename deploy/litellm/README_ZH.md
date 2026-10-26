# 本地 LiteLLM 网关

此 Compose 部署栈运行 LiteLLM，使用 PostgreSQL 持久化数据，并启用管理界面（Admin UI）。
网关默认绑定 `127.0.0.1:4000`。PostgreSQL 默认在宿主机回环地址发布 `5432` 端口，
也可显式配置为通过可信的外部网络接口访问。

## 启动网关

创建本地环境文件，并替换所有示例密钥：

```bash
cp deploy/litellm/.env.example deploy/litellm/.env
openssl rand -hex 32
```


为 `LITELLM_MASTER_KEY`、`LITELLM_SALT_KEY`、`LITELLM_UI_PASSWORD`
和 `POSTGRES_PASSWORD` 分别生成独立的值。主密钥须以 `sk-` 开头。
保存提供商凭据后，不要轮换 `LITELLM_SALT_KEY`：新的盐值无法解密已有的加密凭据。

启动并检查部署栈：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml ps
curl http://127.0.0.1:4000/health/liveliness
```


在 `.env` 中设置 `COMPOSE_PROFILES=bundled-db`，可确保执行 `up` 和 `down` 时
PostgreSQL 都属于当前启用的 Compose 服务集。若只在执行 `up` 时指定该配置档，
之后直接执行 `down` 可能因未选中 PostgreSQL 服务而使其继续运行。

本地部署默认只启动一个 LiteLLM 工作进程，并关闭遥测。Compose 同时设置资源上限：
LiteLLM 为 1 CPU / 2 GiB，模型同步服务为 0.25 CPU / 128 MiB，
PostgreSQL 为 0.5 CPU / 256 MiB。生产并发需要更大容量时，可覆盖
`LITELLM_CPU_LIMIT`、`LITELLM_MEMORY_LIMIT` 及 `.env.example` 中的相关配置。
交换空间不能超过各服务的内存上限。上限过低可能导致容器内存不足并被 Docker 重启。

### Langfuse LLM 生成记录

代理除 SMTP 告警外，还启用了 LiteLLM 的 `langfuse_otel` 回调。
请配置为与应用 OTel Collector 使用相同的 Langfuse 项目：

```dotenv
LANGFUSE_PUBLIC_KEY=pk-lf-replace-me
LANGFUSE_SECRET_KEY=sk-lf-replace-me
LANGFUSE_OTEL_HOST=https://langfuse.example.com
```


`LANGFUSE_OTEL_HOST` 只填写基础主机地址。LiteLLM 会自动追加 `/api/public/otel`，
不要在该变量中包含此路径。成功和失败的模型调用都会导出为 Langfuse Generations，
包含请求与响应、Token 用量、耗时、模型和应用提供的任务元数据。
这些数据可能包含用户内容和提示词，应相应配置 Langfuse 的保留策略与访问控制。

修改这些值或回调列表后，只需重建 LiteLLM，无需重启模型同步服务：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml \
  up -d --no-deps --force-recreate litellm
```


### OpenAI 提供商

可直接配置 OpenAI，也可通过兼容的私有端点路由 OpenAI 模型：

```dotenv
OPENAI_API_KEY=sk-replace-with-your-key
OPENAI_BASE_URL=https://api.openai.com/v1
```


使用兼容代理时：

```dotenv
OPENAI_BASE_URL=https://openai-proxy.example.com/v1
```


固定版本的 LiteLLM 镜像也识别旧变量 `OPENAI_API_BASE`，但 `OPENAI_BASE_URL`
优先级更高，也是此 Compose 部署采用的配置约定。模型应使用 `openai/` 提供商前缀。

### Groq 提供商

Compose 服务将 LiteLLM 原生的 Groq 环境变量传入网关：

```dotenv
GROQ_API_KEY=gsk-replace-with-your-key
GROQ_API_BASE=https://api.groq.com/openai/v1
```


`GROQ_API_BASE` 可以指向兼容的私有端点或网络代理。
地址应包含目标端点要求的 Groq OpenAI 兼容路径 `/openai/v1`，末尾不要加斜杠。
在 LiteLLM 中注册的模型应使用 `groq/` 提供商前缀，
例如 `groq/llama-3.3-70b-versatile`。

### Gemini 提供商

Compose 服务也会传入 LiteLLM 原生的 Gemini 端点变量：

```dotenv
GEMINI_API_KEY=replace-with-your-key
GEMINI_API_BASE=https://generativelanguage.googleapis.com
```


使用兼容的私有端点或网络代理时，将 `GEMINI_API_BASE` 替换为其基础源地址。
标准配置不要包含 `/v1beta`，因为 LiteLLM 会自动追加 Gemini API 路径：

```dotenv
GEMINI_API_BASE=https://gemini-proxy.example.com
```


在 LiteLLM 中注册的模型应使用 `gemini/` 提供商前缀，
例如 `gemini/gemini-2.5-flash`。

### xAI 提供商

可直接配置 xAI，也可将其 OpenAI 兼容端点替换为私有代理：

```dotenv
XAI_API_KEY=xai-replace-with-your-key
XAI_API_BASE=https://api.x.ai/v1
```


在 LiteLLM 中注册的模型应使用 `xai/` 提供商前缀，例如 `xai/grok-4`。

### NVIDIA NIM 提供商

可直接配置 NVIDIA NIM，也可将 LiteLLM 指向兼容的自托管 NIM 端点：

```dotenv
NVIDIA_NIM_API_KEY=nvapi-replace-with-your-key
NVIDIA_NIM_API_BASE=https://integrate.api.nvidia.com/v1
```


在 LiteLLM 中注册的模型应使用 `nvidia_nim/` 提供商前缀。
自托管端点可使用类似 `http://nvidia-nim.internal:8000/v1` 的地址。

打开管理界面 `http://127.0.0.1:4000/ui`，使用 `LITELLM_UI_USERNAME`
和 `LITELLM_UI_PASSWORD` 登录。Swagger 文档仍位于 `http://127.0.0.1:4000/docs`。

本地直连配置不要设置公共 URL 前缀：

```dotenv
SERVER_ROOT_PATH=
PROXY_BASE_URL=http://127.0.0.1:4000
```


若改为设置 `SERVER_ROOT_PATH=/litellm`，直接访问容器管理界面的地址为
`http://127.0.0.1:4000/litellm/ui/`；访问 `http://127.0.0.1:4000/ui/`
会返回 404，因为界面资源挂载在配置的根路径下。

### 在 `/litellm` 子路径下提供服务

公共网关通过 URL 前缀暴露 LiteLLM 时，需要同时配置 ASGI 根路径和外部可访问的 URL：

```dotenv
SERVER_ROOT_PATH=/litellm
PROXY_BASE_URL=https://gateway.example.com/litellm
```


Compose 中保留 `DOCS_URL=/docs` 和 `ROOT_REDIRECT_URL=/ui`。
LiteLLM 生成公共管理界面路由时会应用 `SERVER_ROOT_PATH`，
若在这两个值中也加入前缀，会导致路径重复。

使用 `nginx-litellm-subpath.conf` 作为 Nginx location 模板。
其中 `proxy_pass` URL 末尾的 `/` 用于在转发前移除外部 `/litellm/` 前缀。
如果服务配置了类似 `location ~ /\.` 的通用隐藏文件规则，还必须保留 `^~`
location 修饰符；否则 `/.well-known/litellm-ui-config` 可能在到达 LiteLLM 前
就被 Nginx 拒绝。

将 location 配置放入对应的 HTTPS `server` 后，校验并重载 Nginx，
然后重建 LiteLLM 以加载新的环境变量：

```bash
sudo nginx -t
sudo systemctl reload nginx
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d --force-recreate litellm
```


同时验证直连上游路由和带前缀的公共路由：

```bash
curl -i http://127.0.0.1:4000/health/liveliness
curl -i http://127.0.0.1:4000/.well-known/litellm-ui-config
curl -i https://gateway.example.com/litellm/health/liveliness
curl -i https://gateway.example.com/litellm/.well-known/litellm-ui-config
```


公共地址的响应应来自 LiteLLM，而不是 Nginx 的 HTML 404 页面。
旧版本曾出现 `SERVER_ROOT_PATH` 回归，因此升级固定版本的 LiteLLM 镜像时，
应在部署前验证子路径下的管理界面行为。

### SMTP 邮件通知

Compose 服务从 `.env` 传入可选的 SMTP 配置。
使用腾讯企业邮箱时，请配置完整的发件人地址和客户端专用密码：

```dotenv
SMTP_HOST=smtp.exmail.qq.com
SMTP_PORT=465
SMTP_USERNAME=notifications@example.com
SMTP_PASSWORD=replace-with-client-specific-password
SMTP_SENDER_EMAIL=notifications@example.com
SMTP_TLS=False
SMTP_USE_SSL=True
LITELLM_EMAIL_INCLUDE_API_KEY=False
SERVER_ROOT_PATH=/litellm
PROXY_BASE_URL=https://proxy.your-company.com/litellm
```


`PROXY_BASE_URL` 必须是外部可访问的 LiteLLM URL，因为通知模板会使用它生成链接。
非本地环境优先使用 HTTPS，末尾不要加斜杠。

完成所有 SMTP 配置后，在 `config.yaml` 中启用回调：

```yaml
litellm_settings:
  callbacks: ["smtp_email"]
```


修改任一文件后均需重启 LiteLLM。SMTP 回调会向相关事件绑定的邮箱发送邀请、
虚拟密钥和预算通知，不用于记录请求或响应载荷。

通过管理界面创建的模型、虚拟密钥和提供商凭据保存在 `litellm_postgres_data` 数据卷中。
`STORE_MODEL_IN_DB=True` 可让界面管理的模型在重启后继续保留。

### 模型初始化与管理界面的配置管理范围

基础模型别名定义在 `bootstrap-models.json` 中，而不是 LiteLLM 的 `model_list`。
`model-bootstrap` 服务等待 LiteLLM 健康后，检查 `/model/info`，
通过 `POST /model/new` 创建缺失的部署，并通过 `PATCH /model/{id}/update`
更新清单负责管理且发生变化的 `litellm_params`。模型存储在 PostgreSQL 中，
在管理界面中显示为 `db_model=true`。

同步服务对 `litellm_params` 采用声明式管理，其他配置则尽量保留：

- 通过公开模型名、上游模型和 Credential 匹配已有部署；
  清单中的 `litellm_params` 字段与现有值不同时，会进行更新。
- 保留 LiteLLM 生成的默认值，以及不由清单管理的 `model_info` 字段。
- 不删除种子清单之外的模型。
- 被删除的基础别名会在下一轮同步时重新创建。
- 提供商 API Key 仍通过环境变量引用，不复制到种子清单中。

#### 对任意 JSON 清单执行单次同步

在仓库根目录运行以下命令，同步新增或修改后的清单：

```bash
./deploy/litellm/sync-models.sh deploy/litellm/tencent-models.json
```


命令需要 Python 3、Docker Compose、Compose 的 `.env` 文件，以及正在运行的
`litellm` 服务。主密钥在该容器内部读取，不通过命令参数传递，也不会打印。
无需运行 `model-bootstrap` 服务，无需额外挂载数据卷或重启容器。

JSON 格式与 `bootstrap-models.json` 或 `qwen-models.json` 相同。
引用的 LiteLLM Credential 必须预先存在。例如：

```json
[
  {
    "model_name": "my-model",
    "litellm_params": {
      "model": "upstream-model",
      "custom_llm_provider": "custom_openai",
      "litellm_credential_name": "existing-credential"
    },
    "model_info": {"id": null}
  }
]
```


可以传入多个文件一起同步，也可以指定其他 Compose 环境文件：

```bash
./deploy/litellm/sync-models.sh path/to/first.json path/to/second.json
./deploy/litellm/sync-models.sh --env-file deploy/litellm/.env path/to/models.json
```


清单路径相对于当前目录解析。命令会在写入模型前校验所有文件，并检查命名 Credential。
随后创建缺失的部署、更新清单管理且发生变化的参数，再回读已保存的配置进行核对。
输出 JSON 字段 `created`、`updated` 和 `verified`，失败时以非零状态码退出。
写入不具备事务性：失败时可能已有部分模型完成同步；修正原因后重新运行即可。
清单之外的现有模型会保留。命令不发送推理请求。

此命令只运行一次，不会将 JSON 加入周期同步服务。
如果周期同步服务也管理相同别名，应保持其清单一致，避免下一轮同步撤销本次变更。
前述已有部署匹配规则和 Team 归属规则仍然适用。

#### 使用关联 LiteLLM Credential 的 Qwen 清单

`qwen-models.json` 是仅包含 Qwen 模型的清单，可替代 `bootstrap-models.json`。
它引用名为 `dashscope-qwen` 的可复用 LiteLLM Credential，
并将模型分配给别名为 `ai-agent` 的 Team：

```json
{
  "team_name": "ai-agent",
  "litellm_params": {
    "model": "dashscope/qwen3.7-plus",
    "litellm_credential_name": "dashscope-qwen",
    "drop_params": true
  }
}
```


`drop_params: true` 会让 LiteLLM 丢弃所选自定义提供商不支持的请求参数，
而不是拒绝请求。同步服务会为缺失的部署添加此设置，
并更新尚未配置该字段的已有部署。

选择此清单前，先在 LiteLLM 管理界面创建 `dashscope-qwen`。
该 Credential 应包含 DashScope API Key 和 API 基础地址。
其名称是 LiteLLM 数据库查询使用的字面键值；在 `/model/new` 载荷中，
不能用 `credential_name` 或 `credential_id` 替代。

启动同步服务前先创建 Team，或将 `qwen-models.json` 中的 `team_name`
改为已有的 LiteLLM `team_alias`。`team_name` 是本地清单提供的便利字段：
同步服务调用 `GET /team/list`，要求别名精确且唯一匹配，然后移除 `team_name`，
将解析后的 ID 作为 `model_info.team_id` 发送到 `POST /model/new`。

省略 `team_name`，或将其设为 `null`、`""` 或空白字符串，
即可创建全局模型，无需解析或分配 Team。仅供初始化使用的 `team_name`
字段总会在请求 `/model/new` 前移除。显式将 `team_name` 设为空值或 null，
还会移除过时的 `model_info.team_id`；完全省略 `team_name` 时，
则保留显式配置的 `model_info.team_id`。

清单允许重复使用同一 `model_name`，以创建包含多个部署的 LiteLLM 模型组，
例如每个 Credential 对应一个部署。重复条目的上游 `model`、
`litellm_credential_name` 或 Team 必须有所不同；
只有部署元组完全相同时，才会被作为配置错误拒绝。

LiteLLM 将通过 `model_info.team_id` 限定范围的模型限制在 Premium/Enterprise 部署中使用。
Community 部署创建 Team 级模型时会返回 HTTP 403；
可移除各条目的 `team_name`，改为创建普通全局模型。

确认 Credential 已存在：

```bash
curl -sS \
  -H "Authorization: Bearer <LITELLM_MASTER_KEY>" \
  http://127.0.0.1:4000/credentials/by_name/dashscope-qwen
```


确认配置的 Team 别名已存在：

```bash
curl -sS \
  -H "Authorization: Bearer <LITELLM_MASTER_KEY>" \
  http://127.0.0.1:4000/team/list
```


在 `deploy/litellm/.env` 中配置同时同步基础清单和 Qwen 清单：

```dotenv
LITELLM_MODEL_BOOTSTRAP_MANIFESTS=/bootstrap/bootstrap-models.json,/bootstrap/qwen-models.json
```


`LITELLM_MODEL_BOOTSTRAP_MANIFESTS` 接受逗号分隔的清单列表，
优先级高于为向后兼容保留的单清单变量 `LITELLM_MODEL_BOOTSTRAP_MANIFEST`。
这样可避免仅选择 Qwen 文件时，基础文件的变更被静默忽略。

然后重建同步服务：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d --force-recreate model-bootstrap
```


如果 `agent-planner` 等别名已存在，选择 Qwen 清单后会更新声明的 `litellm_params`，
包括 Credential、提供商、上游模型和 `drop_params`。
不会删除无关部署，也不会替换无关的 `model_info` 设置。

每轮同步都会重新加载清单。绑定挂载缺失、JSON 无效、Team 未知，
或 LiteLLM API 暂时不可用时，服务会记录清单路径并重试，不会终止容器。
若 Docker 显示 `Restarting`，请确认已部署的 `bootstrap_models.py`
包含此重试行为，再重建 `model-bootstrap`；旧脚本会在进入重试循环前加载清单。

HTTP 失败日志包含 LiteLLM 端点及最多 4 KiB 的响应正文。
可利用这些信息区分无效 Credential 与 Team 的 Premium/Enterprise 限制，
而不只依赖状态码判断原因。

默认别名使用不同的 DashScope Qwen 模型，支持结构化输出：

| 别名 | DashScope 模型 | 用途 |
| --- | --- | --- |
| `agent-planner` | `qwen3.7-plus` | 复杂规划与工具决策 |
| `agent-planner-fallback` | `qwen3.6-plus` | 规划器回退 |
| `agent-writer` | `qwen3.5-plus` | 最终综合与写作 |
| `agent-fast` | `qwen3.6-flash` | 低延迟辅助场景 |
| `agent-embedding` | `text-embedding-v4` | 记忆与检索向量 |

Legacy 别名同时保留，用于受控回退和回滚：

| 别名 | 上游模型 |
| --- | --- |
| `agent-planner-legacy` | `openai/gpt-4.1-mini` |
| `agent-planner-fallback-legacy` | `gemini/gemini-2.5-flash` |
| `agent-writer-legacy` | `openai/gpt-4.1-mini` |
| `agent-fast-legacy` | `gemini/gemini-2.5-flash` |

推理前配置 DashScope 凭据和区域端点：

```dotenv
DASHSCOPE_API_KEY=sk-your-dashscope-key
DASHSCOPE_API_BASE=https://dashscope.aliyuncs.com/compatible-mode/v1
OPENAI_API_KEY=sk-your-openai-key
GEMINI_API_KEY=your-gemini-key
```


根目录的 `config.yaml` 将核心场景绑定到 Qwen 别名，
并使用对应的 Legacy 别名作为应用层回退。若要确定性地切换某个场景，
可交换其主模型和回退模型名称。例如：

```yaml
llm:
  scenes:
    task_planner:
      model: "agent-planner-legacy"
      fallback_scene: "task_planner_qwen_fallback"
    task_planner_qwen_fallback:
      model: "agent-planner"
```


修改场景绑定后，重载 ai-agent：

```bash
curl -X POST \
  -H "Authorization: Bearer ${AI_AGENT_API_KEY}" \
  http://127.0.0.1:8088/api/config/reload
```


在 LiteLLM 管理界面修改别名背后的上游部署后，变更会直接生效，无需重载 ai-agent。
需要确定性切换和回滚时，应为不同提供商保留独立别名；
若在同一别名下注册多个提供商，则由 LiteLLM 路由决定选择结果。

默认同步间隔为 60 秒，可在 `deploy/litellm/.env` 中覆盖：

```dotenv
LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS=60
LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS=10
```


查看初始化同步日志，并确认模型由数据库管理：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml logs model-bootstrap

source deploy/litellm/.env
curl -sS -H "Authorization: Bearer ${LITELLM_MASTER_KEY}" \
  http://127.0.0.1:4000/model/info |
  jq '.data[] | {model_name, db_model: .model_info.db_model}'
```


停止服务但保留数据库：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml down
```


除非明确需要丢弃持久化的 LiteLLM 配置、密钥和费用历史，否则不要使用 `down --volumes`。

## 对外开放内置 PostgreSQL 服务

内置 PostgreSQL 服务通过 `POSTGRES_BIND_ADDRESS` 和 `POSTGRES_PORT`
发布容器端口。默认只允许通过宿主机回环地址访问：

```dotenv
POSTGRES_BIND_ADDRESS=127.0.0.1
POSTGRES_PORT=5432
```


需要从可信外部网络访问时，尽量绑定服务器的私网或 VPN 地址。
也支持绑定所有网络接口，但必须配合严格的防火墙或云安全组规则：

```dotenv
POSTGRES_BIND_ADDRESS=0.0.0.0
POSTGRES_PORT=55432
POSTGRES_PASSWORD=replace-with-a-long-random-password
```


修改端口映射后，只重建 PostgreSQL 容器即可，命名数据卷会保留：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d --force-recreate postgres
```


远程客户端应连接 Docker 宿主机，而不是 Compose 服务名：

```bash
psql "host=your-server.example.com port=55432 dbname=litellm user=litellm sslmode=prefer"
```


不要将 PostgreSQL 无限制地暴露到公网。入站 TCP 仅允许已知来源 IP，
使用非默认宿主机端口，并优先通过 VPN 或 SSH 隧道访问。
内置配置不会提供 PostgreSQL 服务端证书，因此在通过不可信网络发送凭据前，
应另行配置 PostgreSQL TLS。

## 使用已有的 PostgreSQL 服务器

LiteLLM 可以与其他应用共享 PostgreSQL 服务器，但应使用独立数据库和数据库用户，
不要共享其他应用的数据库或 schema。该用户必须有权创建和迁移 LiteLLM 表。

在 `deploy/litellm/.env` 中设置 `LITELLM_DATABASE_URL`。
例如，PostgreSQL 运行在 Docker 宿主机上，监听宿主机 `55432` 端口时：

```dotenv
COMPOSE_PROFILES=
LITELLM_DATABASE_URL=postgresql://litellm:password@host.docker.internal:55432/litellm
```


URL 中用户名或密码包含特殊字符时，应进行百分号编码。
`host.docker.internal` 可用于 Docker Desktop；
Compose 配置也通过 `host-gateway` 为 Linux 上的 Docker Engine 映射此名称。

不启用 `bundled-db` 配置档，启动 LiteLLM 及其模型同步服务：

```bash
docker compose --env-file deploy/litellm/.env \
  -f deploy/litellm/compose.yaml up -d litellm model-bootstrap
```


若 PostgreSQL 位于其他机器，将 `host.docker.internal:55432`
替换为该服务器可访问的主机名和端口。确保 PostgreSQL 的 `listen_addresses`、
`pg_hba.conf` 和防火墙规则允许 Docker 宿主机或其网络访问。

## 连接 ai-agent

运行项目客户端，通过网关调用模型：

```bash
set -a
source deploy/litellm/.env
set +a
AI_AGENT_LLM_PROVIDER=litellm \
AI_AGENT_LLM_MODEL=agent-planner \
AI_AGENT_LLM_BASE_URL=http://127.0.0.1:4000/v1/chat/completions \
AI_AGENT_LLM_API_KEY="$LITELLM_MASTER_KEY" \
go run ./cmd/llm-eval -input Sample/llm-eval.jsonl
```


就绪检查默认使用 `gateway` 模式：服务检查 LiteLLM 存活状态和模型可见性，
不发送收费的生成请求。需要严格推理验证时，显式配置：

```bash
AI_AGENT_LLM_READINESS_MODE=inference \
AI_AGENT_LLM_READINESS_CACHE_TTL_SECONDS=300 \
go run ./cmd/server
```


可用模式为 `config_only`、`gateway` 和 `inference`。
只有 `inference` 探测成功后才会设置 `llm_verified=true`，且该探测可能产生提供商费用。
`/ready` 通过 `llm_readiness_mode` 返回当前模式。

每个用例默认超时为 30 秒。CI 中可使用 JSON Lines 输出；
全部用例通过时命令退出码为 `0`，评测失败为 `1`，参数或输入无效为 `2`：

```bash
go run ./cmd/llm-eval \
  -input Sample/llm-eval.jsonl \
  -timeout 45s \
  -max-total-tokens 10000 \
  -input-cost-per-million-usd 2 \
  -output-cost-per-million-usd 8 \
  -max-total-cost-usd 1.50 \
  -format json
```


评测用例需要超过默认 4 MiB 的 JSONL 单行限制时，可使用 `-max-line-bytes`。

每个 JSONL 用例必须且只能选择一种断言：`expected_contains` 用于不区分大小写的
子字符串匹配，`expected_exact` 用于精确匹配，`expected_regex` 用于 Go 正则表达式匹配。
JSON 答案也可以同时设置 `expected_json_path` 和 `expected_json_value`；
路径支持根节点 `$`、点分隔的对象字段，以及非负数组下标，例如 `$.items[0].status`。
期望值必须是有效的 JSON 值。`-max-total-tokens` 为正数时，达到预算后不会再启动下一用例；
默认值 `0` 表示不启用此限制。

使用 `-parallelism N` 可通过有界工作池并行评测。首次调用 LLM 前会校验全部输入，
结果保持 JSONL 输入顺序。并行执行不能与 `-max-total-tokens` 同时使用，
因为已发出的调用无法遵守严格的总 Token 预算。

费用估算使用传入的每百万输入和输出 Token 单价，CLI 不内置提供商价格表。
对于使用不同定价模型的用例，可通过 `input_cost_per_million_usd`
和 `output_cost_per_million_usd` 覆盖全局单价。
`-max-total-cost-usd` 要求至少一个单价非零，且与严格 Token 预算一样，不能用于并行执行。

定性评测可设置 `judge_criteria`，替代文本或 JSON 断言。
评审默认使用 `answer_verifier` 场景，要求分数至少为 `0.7`，并返回分数和理由。
可通过 `judge_scene` 和 `judge_min_score` 覆盖默认值。
如果评审模型价格不同，可设置 `judge_input_cost_per_million_usd`
和 `judge_output_cost_per_million_usd`；评审用量计入用例的 Token 和费用预算。

即使从 `deploy/e2e` 启动，服务仍会加载仓库根目录的 `.env`。
因此，当根目录 `.env` 选择其他提供商时，需要显式设置 `AI_AGENT_LLM_PROVIDER=litellm`。

`deploy/e2e/config.yaml` 会故意调用不存在的主模型，再回退到 `agent-planner`。
从该目录构建并运行，可验证应用层回退：

```bash
go build -o /tmp/ai-agent-llm-eval ./cmd/llm-eval
cd deploy/e2e
AI_AGENT_LLM_API_KEY="$LITELLM_MASTER_KEY" \
  /tmp/ai-agent-llm-eval -input ../../Sample/llm-eval.jsonl
```


Google 可能拒绝来自不受支持地区的 Gemini API 调用。这属于外部提供商限制；
基于 OpenAI 的别名仍可用于本地网关和回退测试。
