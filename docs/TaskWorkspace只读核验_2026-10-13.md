# Task workspace 只读核验

日期：2026-10-13。实现与验收记录，未部署。承接 [只读恢复准入评估](Task只读恢复准入评估_2026-10-13.md)。已接入实际文件检查器；workspace 核验通过可以出现在 verified_evidence，但整体仍 decision=blocked、restore_ready=false、source_preserved=true。

## 根目录与操作者基准

检查器只读以下固定目录，不接受证据 JSON 内的绝对根目录或服务器路径：

```text
<已有专用恢复沙箱>/workspaces/<SHA-256(tenant_id)>/<SHA-256(task_id)>/
```

两个目录名均为 ID 原始 UTF-8 字节的 64 位小写十六进制 SHA-256。根目录由目标 Task/tenant 决定，不能通过 file.path 更改；目标数据库仍须通过专用身份、私有权限、只读重开和独立记录核验。不同租户/任务的目录分开，未提供独立基准时不会扫描这些目录。

沙箱文件需由操作者预先准备；本命令不复制源 workspace、不创建目录、不改写文件、不删除产物。所有相关目录及文件须禁止 group/other 权限，拒绝符号链接、硬链接文件、特殊文件及 setuid/setgid/sticky 标志。文件可用 0400/0600/0500/0700 权限，证据必须与实际权限一致。

`--workspace-evidence` 是操作者独立选择的文件基准，不能从上传依赖清单中提取。操作者必须核对来源与完整性。检查器证明的是专用目录与此基准的完整文件清单一致；它不能证明原源 workspace 未遗漏其他依赖，不验证基准的签名，也不会从目标目录自行生成基准作为“来源证明”。

## 基准格式

schema=task-restore-workspace-files-v1，最多 256 KiB。示例中的 hash 和时间需要替换为实际值；mode 使用 JSON 十进制，384 对应 Unix 0600：

```json
{
  "schema": "task-restore-workspace-files-v1",
  "task_id": "task-id",
  "tenant_id": "tenant-a",
  "payload_sha256": "<对应 v2 包的 64 位 SHA-256>",
  "created_at": "2026-10-13T12:00:00Z",
  "expires_at": "2026-10-13T12:05:00Z",
  "files": [
    {
      "path": "inputs/data.csv",
      "size": 1200,
      "sha256": "<文件内容的 64 位 SHA-256>",
      "mode": 384
    }
  ]
}
```

Task/tenant/payload 必须匹配专用沙箱。files 必须明确提供数组，按 path 严格升序且唯一；空数组表示操作者基准要求目录中没有任何文件。拒绝未知/重复 JSON 键、尾随 JSON、非法 hash、绝对路径、`.`/`..` 路径成分、反斜杠、冒号和控制字符。路径长度最多 512，最多 1024 个文件，声明总大小最多 64 MiB。核验时间窗口最多 15 分钟，过期或未来基准不能列入已核验证据。

实际扫描另限制最多 4096 个目录项、目录深度 32、每轮累计读取最多 64 MiB；读取以 32 KiB 缓冲进行并检查取消。大小/hash/权限不符或额外文件报告 conflict，缺失文件/根目录报告 missing；没有支持的平台或基准时不能获得 verified。

## 使用与报告

```bash
/private/path/task-restore assess \
  --directory /private/path/restore-sandboxes/task-restore-XXXXXX \
  --task task-id --tenant tenant-a \
  --workspace-evidence /private/path/workspace-baseline.json

# 可选：同时提供依赖摘要清单。
/private/path/task-restore assess \
  --directory /private/path/restore-sandboxes/task-restore-XXXXXX \
  --task task-id --tenant tenant-a \
  --workspace-evidence /private/path/workspace-baseline.json \
  --manifest /private/path/dependencies.json
```

未提供 --manifest 时，程序在内存生成当前绑定和 workspace 摘要声明，随后实际扫描核验；没有自动创建基准。提供 --manifest 时 workspace 声明的 sha256 必须为操作者基准文件原始字节的 SHA-256。仅添加摘要声明而不提供独立基准，仍然是 unverified。

workspace 接入后，policy_version 为 read-only-workspace-v1，state_revision 同时绑定 SQLite 文件指纹、基准摘要、实际文件内容/权限/文件身份/修改时间/变更时间以及目录结构。原数据库单独绑定不能直接复用于此评估；需要当前报告的 binding。评估前、workspace 证据核验时及结束时重读实际状态；清单失效或状态变化时不列出本次已核验证据；单条过期证据不列入已核验证据，并报告拒绝原因。

成功核验一个类别的输出片段：

```json
{
  "verified_evidence": [
    {"kind": "workspace", "sha256": "<基准文件摘要>"}
  ],
  "evidence_complete": false,
  "decision": "blocked",
  "restore_ready": false,
  "source_preserved": true
}
```

整体阻止结果仍退出 3；参数错误 2；输入/读取/核验失败 1。其余类别仍需相应的可信检查器；空清单按 missing 报告，只有摘要的声明按 unverified 报告。报告只显示类别和摘要，不输出相对文件名、内容、基准路径或原始文件系统错误。

## 文件隔离实现与边界

实际读取通过 [restoreworkspace policy](../internal/policy/restoreworkspace/scan.go)，在固定根内逐级使用 os.Root 固定目录句柄，检查 named/pinned identity；叶文件使用 O_NOFOLLOW|O_NONBLOCK 只读打开，防止叶链接跟随与替换为 FIFO 后阻塞。读取前后核对身份、大小、权限和修改/变更时间，目录扫描结束后复查目录身份，完整文件清单按稳定顺序比对。内部版本不包含 access time，避免读取自身使指纹失效。

本机 Darwin/amd64 实际测试通过；Linux/amd64 分支仅交叉编译通过，本轮未在 Linux 运行。Windows 分支编译通过，检查器明确拒绝；其他非 Darwin/Linux 平台同样不授予 workspace verified。检查器不建立 mount namespace，不防御具有宿主挂载权限的操作者，也不提供文件系统快照或在线执行的原子 fencing。操作系统可能更新访问时间；“只读”指不改写文件内容、权限或创建/删除产物。

## 验收与后续

回归覆盖实际目录/内容指纹稳定性、跨租户、缺失/额外/大小/hash/权限不符、非法路径/基准字段、符号链接/硬链接/FIFO、字节配额、取消、读取中修改及评估前后修改；数据库、文件、基准字节和目录项保持不变。离线命令验证已核验摘要输出、整体退出码 3、正文/文件名/路径不泄露及旧绑定拒绝。

```bash
go test -p 1 ./... -timeout 90s
go test -race ./internal/policy/restoreworkspace ./internal/store ./internal/api ./cmd/task-restore -timeout 90s
go vet ./internal/policy/restoreworkspace ./internal/store ./internal/api ./cmd/task-restore
git diff --check
```

[本批次验收记录](benchmarks/task-workspace-restore-2026-10-13.json)。下一步接入历史密钥版本与审批密文的只读检查器，随后是外部审计、session/租约、审批副作用和索引；源 workspace 的权威完整清单与签名来源仍需后续完善。PostgreSQL 在线验收仍需专用 DSN；在线恢复、发布、执行切换与源记录移除尚未实施。

## 提交与下次续作状态

代码已提交至本地 `5052421`（Store 数据基础、联合导出、离线恢复/准入/workspace 核验及测试），未推送、未部署；HTTP 管理员入口及其他工作区改动未纳入该提交。下一次优先按 [密钥管理只读核验续作](Task密钥管理只读核验续作_2026-10-13.md) 实施，密钥检查器当前尚未实现。本轮仅同步文档，文档更新尚未提交；不把数据保真当作解密验收，继续 blocked、restore_ready=false、source_preserved=true。
