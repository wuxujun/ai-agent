# 独立 API Access Log

所有通过主 HTTP Router 的请求现在都会写入独立的每日 JSON 文件：

```text
logs/access-YYYY-MM-DD.log
```

通过 `log.access_enabled` 或环境变量 `AI_AGENT_LOG_ACCESS_ENABLED` 开关控制，
目录和保留周期复用 `log.directory`、`log.retention_days`。配置热加载会同步重建
access logger，无需重启服务。

记录字段包括 `request_id`、`method`、`path`、匹配的路由模板、HTTP 状态码、
耗时、客户端 IP、响应字节数、User-Agent，以及认证成功后可用的 `tenant_id` 和
OpenTelemetry `trace_id`。缺少或不合法的 `X-Request-ID` 会被替换为服务端 UUID，
并通过响应头返回。

普通级别日志、task-report 和 access log 均自动包含当前二进制的 `app_version`。
本地未注入版本的构建使用 `dev`；发布构建通过 Go linker 的 `-X` 参数写入版本。

为避免凭据和业务内容泄露，access log 不记录查询字符串、Authorization、
X-API-Key、Cookie、请求体或响应体。正常 API、认证失败、健康检查与 404 均会
产生记录；流式/SSE 请求在连接结束后写入最终状态和持续时间。
