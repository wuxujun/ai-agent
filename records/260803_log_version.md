# 全量日志应用版本字段

所有通过 `internal/logger` 输出的 JSON 记录统一增加 `app_version`，覆盖控制台、
DEBUG/INFO/WARN/ERROR 每日文件、task-report 和 access log。标准库 `log` 经
`slog.SetDefault` 桥接后也带有相同字段。

版本由 `internal/buildinfo` 提供。源码运行或未注入的构建使用 `dev`；`build.sh`
使用 `VERSION`，GitHub Release 使用 workflow 输入或 Git Tag，通过以下 linker
变量注入：

```text
github.com/wuxujun/ai-agent/internal/buildinfo.Version
```

Gin 的重复文本 access logger 已移除。默认 Recovery 也替换成结构化 Recovery，
避免 panic 堆栈成为缺少版本字段的文本日志，同时避免默认行为输出请求 Header。
panic 请求仍会在 access log 中记录最终 `500`。
