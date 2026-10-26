# PostgreSQL 数据备份

`backup-postgres.py` 备份 ai-agent 的单个 PostgreSQL 数据库。不会停止服务、修改源库业务数据、恢复数据库或删除历史文件。要求 Python 3.9+；本机模式另需匹配数据库版本的 `pg_dump` / `pg_restore`，容器模式使用指定容器中的客户端。

## 当前本机环境

从仓库根目录执行，仅检查输入与客户端入口，不连接数据库、不创建目录：

```sh
python3 deploy/backup/backup-postgres.py \
  --env-file .env --docker-container paradedb --dry-run
```

实际备份命令：

```sh
python3 deploy/backup/backup-postgres.py \
  --env-file .env --docker-container paradedb
```

本次仅执行了上述 dry-run，未备份业务库。`paradedb` 是本机已观察到的容器名；在其他机器上需明确指定对应容器。容器模式不自动发现或切换目标：DSN 中的地址必须从该容器内可达。例如容器内 `127.0.0.1:5432` 指容器自身数据库，不是宿主机的映射端口。

本机安装 PostgreSQL 客户端后，也可省略 `--docker-container`。非 PATH 安装可指定 `--pg-bin-dir /absolute/path/to/postgresql/bin`。

## 配置与凭据

- 必须明确设置 `AI_AGENT_STORE_TYPE=postgres`（也接受 postgresql）及 `AI_AGENT_STORE_DSN`。接受 PostgreSQL URI 或 libpq keyword/value DSN，必须显式包含 host、dbname、user，避免默认连到另一个库。
- 默认只读取进程环境；指定 `--env-file` 才读文件。进程中的同名变量优先于文件。**不会读取 YAML、自动寻找 `.env` 或导入应用的其他密钥。** 如果运行服务仅靠 YAML 配置，先通过受控环境提供上述两个变量。
- dotenv 支持单行字面量、单/双引号、行尾注释和 export 前缀；不执行 shell，不展开变量。双引号转义、多行及变量展开表达式会拒绝。复杂值可通过进程环境提供，URI 特殊字符应百分号编码。
- DSN 支持 host、port、dbname、user、password、sslmode、sslrootcert、sslcert、sslkey、application_name、options、connect_timeout、target_session_attrs、channel_binding；其他参数会拒绝，不会静默丢弃。连接超时固定为 10 秒，表锁等待上限 10 秒。
- 连接信息拆成 libpq 环境变量。本机通过子进程环境传入；Docker 通过 stdin 传入静态包装器，再设置容器内进程环境。连接字符串和密码不进入命令参数、清单或控制台输出。不要使用 shell 的 `set -x` 或把凭据直接写进调用命令。
- 证书路径需从实际运行 pg_dump 的环境可读；脚本不复制证书、不放宽 TLS 配置。使用 `.pgpass` 时，同样需放在相应客户端环境中。

`--dry-run` 仅校验配置语法和本地可执行入口，**不会验证容器是否运行、容器内客户端是否存在、数据库连通性、权限或服务端版本**。

## 产物与失败处理

默认保存在 Git 忽略的 `data/backups/`，可用 `--output-dir /absolute/backup/path` 指定独立备份盘。每次新建唯一目录，不覆盖旧产物：

```text
postgres-<UTC时间>-<随机后缀>/
  database.dump
  MANIFEST.json
  SHA256SUMS
```

目录权限 0700、文件权限 0600。`pg_dump --format=custom` 成功、归档头有效且 `pg_restore --list` 可读后，才生成清单、校验和并发布完整目录。脚本不会使用 `pg_restore --dbname` 连接恢复目标。

执行错误、超时、客户端诊断/警告或归档检查失败均返回非零，保留隐藏的 `.postgres-*.partial` 目录及可写时的 `FAILED.json` 阶段标记。原始客户端诊断可能含凭据，因此不输出、不落盘；只显示阶段和受控原因。默认每个客户端阶段最多 1800 秒，可用 `--timeout-seconds` 调整。Docker 客户端超时不保证容器内远端进程立即结束，失败后应在指定容器中核对相关备份会话；脚本不会重启或强杀业务数据库。

成功返回 0，操作失败返回 1，命令参数错误返回 2。没有自动清理或保留期删除逻辑；旧备份由用户按保留策略手动管理。

## 备份验证与恢复范围

成功清单为 `status=archive_checked`、`archive_list_verified=true`、`restore_verified=false`。归档目录检查不验证所有数据块，SHA-256 用来检查后续文件变化；二者都不能替代恢复演练。

在备份目录中核对校验和：

```sh
shasum -a 256 -c SHA256SUMS
```

Linux 也可用 `sha256sum -c SHA256SUMS`。应在独立数据库实际恢复并读回关键数据，确认迁移/回滚兼容，再将其作为升级回退依据。恢复时需准备原对象角色及匹配的 PostgreSQL/扩展环境，尤其是 ParadeDB 和 pgvector。不要直接覆盖正在使用的业务库。

单库备份包含结构与数据，不包含集群级角色/表空间、工作区文件、配置及审批加密密钥。数据库中的审批密文需要原加密密钥或对应旧密钥才能解密，密钥应单独安全备份，不能加入 Git。本脚本未加密归档，备份可能含完整业务敏感数据，应保存在受控存储，并另行配置加密和异机副本。

升级前建议暂停新增任务、等待在途任务结束，然后额外备份一次；脚本不会替用户停任务。日常可由用户配置定时任务，无需为备份每天重启服务；能否只做每日备份取决于可接受的数据损失窗口，较严格要求应配合 WAL/PITR 方案。

官方依据：[pg_dump](https://www.postgresql.org/docs/current/app-pgdump.html)、[pg_restore](https://www.postgresql.org/docs/current/app-pgrestore.html)。

## 测试

不访问数据库的客户端替身回归：

```sh
python3 -m unittest discover -s deploy/backup -p 'test_*.py' -v
```

显式启用真实隔离容器的完整测试：

```sh
AI_AGENT_BACKUP_DOCKER_TEST=1 python3 -m unittest discover -s deploy/backup -p 'test_*.py' -v
```

需要本机已有 `postgres:16-alpine` 镜像；测试使用 `--pull=never`、`--network none`、无宿主端口、tmpfs 数据目录，结束仅停止自身创建的容器。不会连接或备份业务库。文件证据保留在新建临时目录，不批量删除。

2026-10-25：8 项测试通过，其中真实 Docker 备份/恢复读回两条含中文记录，序列恢复后继续生成 id=3。测试首次就绪探针命中初始化临时实例，已修正为最终 TCP 就绪后完整复跑通过；早期失败不计为通过。证据 `/private/var/folders/dw/_vn_xfm95_9fwbgdhbs47z8w0000gn/T/ai-agent-backup-test-6m0euwkl/roundtrip.json`，日志 `/private/tmp/ai-agent-backup-20261025-tests-final.log`。本机原生客户端执行仅通过替身测试；ParadeDB 扩展和业务库恢复尚未验证。
