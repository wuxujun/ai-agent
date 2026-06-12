# Memory / RAG — 向量长期记忆与跨任务知识共享变更记录 (Memory & RAG Changelog)

为了支持长期记忆能力，我们引入了长期向量记忆与跨任务 RAG 上下文注入机制。每当任务成功运行结束，系统都会自动对执行 Trace 事实和最终结论进行摘要向量化存入数据库；新任务启动时自动进行向量检索并在 Planner 阶段作为历史记忆注入。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [memory.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/types/memory.go) | **新增** | 定义 `types.Memory` 结构体，用于保存事实文本、最终答案、时间戳与向量嵌入值 (`Embedding`)。 |
| [task.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/types/task.go) | 修改 | 在 `types.Task` 结构体中新增 `Memories []Memory` 字段。 |
| [store.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/store.go) | 修改 | 扩展 `Store` 接口定义，新增 `SaveMemory` 和 `QueryMemories` 方法。 |
| [memory.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/memory.go) | 修改 | 在内存存储中实现 RAG 检索，支持在线向量余弦相似度（Cosine Similarity）匹配与离线关键字匹配权重评分。 |
| [sqlite.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/sqlite.go) | 修改 | SQLite 表初始化中增加 `memories` 表；实现 `SaveMemory` 插入及 `QueryMemories` 余弦相似度计算与排序。 |
| [postgres.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/postgres.go) | 修改 | PostgreSQL 表初始化增加 `memories` 表；支持冲突更新的插入及语义相近度检索逻辑。 |
| [redis.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/redis.go) | 修改 | Redis 增加 `memory:*` scan/get 读取，并基于内存向量相似度做匹配结果排序。 |
| [embed.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/memory/embed.go) | **新增** | 统一封装 Embedding 模型计算，自动兼容 `gemini` (`text-embedding-004`)、`openai` (`text-embedding-3-small`) 和 `ollama`。内建 **基于 Token Hashing 词频的本地降级算法**，支持 100% 离线和 Mock 环境下的语义相似计算。 |
| [memory.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/memory/memory.go) | **新增** | 实现任务 Trace 智能事实过滤及提取摘要器，并生成 Memory 对象的映射封装。 |
| [memory_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/memory/memory_test.go) | **新增** | 单元测试，覆盖本地及线上嵌入、余弦相似度计算和事实筛选逻辑。 |
| [engine.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/orchestrator/engine.go) | 修改 | 在 `Engine.Next` 拦截器中，新任务首步执行时自动基于 Goal 进行相似记忆提取和注入。 |
| [prompt.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/planner/prompt.go) | 修改 | 在 `BuildUserPrompt` 中格式化 `task.Memories` 为 Planner 可读段落，实现 RAG 知识辅助。 |
| [main.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/cmd/server/main.go) | 修改 | 将初始化后的 Store 实例注入到 Orchestrator `Engine` 字段中。 |
| [rag_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/orchestrator/rag_test.go) | **新增** | RAG 跨任务共享端到端集成测试，验证任务一完成自动变记忆 -> 任务二启动自动读记忆 -> 注入 User Prompt 闭环。 |

---

## ⚙️ 向量记忆配置指南

可以通过如下环境变量调优和切换 Embedding：
```bash
# 指定使用的 Embedding 模型 (默认：Gemini 使用 text-embedding-004，OpenAI 使用 text-embedding-3-small)
export AI_AGENT_EMBEDDING_MODEL="text-embedding-004"
```

如果未提供 API Key，系统将**自动降级**为本地词频嵌入算法。该算法无需网络请求，利用 word Hashing 提取语义成分并做 L2 范数归一化，使用相同的 Cosine Similarity 公式能够正确得到高匹配度得分，极大保证了鲁棒性。

---

## 🧪 测试执行与通过情况

所有的长期记忆与 RAG 上下文共享功能均已通过测试：
```bash
# 运行 Memory RAG 相关单元测试
go test -v ./internal/memory
# 运行 RAG 跨任务上下文传递集成测试
go test -v ./internal/orchestrator -run TestRagMemoryCrossTaskKnowledgeSharing
# 验证整个项目编译与全部测试通过
go test ./...
```
测试执行结果均显示 **PASS**。
