# 存储层接口重构与多后端支持变更记录 (Store Refactoring Changelog)

为了提升 AI Agent 运行时引擎存储层的扩展性与通用性，我们对原有的 SQLite 单一存储方案进行了重构，抽离出通用的 `Store` 接口，并新增了对 **In-Memory**、**PostgreSQL** 以及 **Redis** 的支持。

---

## 📂 新增与修改的文件 (Files Modified)

| 文件路径 | 变更类型 | 描述 |
| :--- | :--- | :--- |
| [go.mod](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/go.mod) | 修改 | 引入 Postgres (`lib/pq`) 与 Redis (`go-redis/v9`) 的驱动包依赖。 |
| [store.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/store.go) | **新增** | 定义统一的 `Store` 接口。 |
| [memory.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/memory.go) | 修改/实现 | 实现线程安全的内存存储（`MemoryStore`），满足本地轻量级运行与快速单元测试。 |
| [postgres.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/postgres.go) | **新增** | 实现 PostgreSQL 存储引擎（`PostgresStore`），支持多实例、高并发、生产级分布式部署。 |
| [redis.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/redis.go) | **新增** | 实现 Redis 存储引擎（`RedisStore`），通过 JSON 序列化快速原子的存取整个 Task 数据，适合高并发缓存。 |
| [store_test.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/store/store_test.go) | 修改/增强 | 编写集成测试套件，全面支持四种数据库引擎的接口存取校验（Postgres 和 Redis 通过环境变量按需启用测试）。 |
| [handler.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/internal/api/handler.go) | 修改 | 将 API 路由处理器引用的具体 `*store.SQLiteStore` 解耦为接口类型 `store.Store`。 |
| [main.go](file:///Users/xujunwu/Documents/IDEAProject/ai-agent/cmd/server/main.go) | 修改 | 主服务启动入口，支持根据环境变量动态实例化相应的存储后端。 |

---

## 🛠️ 重构设计详情 (Refactoring Details)

### 1. `Store` 接口定义
在 `internal/store/store.go` 中，接口提取了 API 和 Engine 所需的最核心数据存取能力：
```go
type Store interface {
	SaveFullTask(ctx context.Context, task *types.Task) error
	GetTask(ctx context.Context, id string) (*types.Task, error)
	Close() error
}
```

### 2. 存储后端实现特点
* **SQLiteStore**: 保留原有设计，基于纯 Go 版本的 SQLite 驱动（无 CGO 依赖），是默认的本地数据持久化方案。
* **MemoryStore**: 内部使用 `sync.RWMutex` 和 `map[string]*types.Task` 实现，在读取与存入时均进行了**深拷贝（Deep Copy）**以杜绝多协程下的并发竞态隐患，检索未找到时返回标准的 `sql.ErrNoRows` 错误。
* **PostgresStore**: 采用标准事务，支持 `ON CONFLICT (id) DO UPDATE SET` 语法来保证 Task 状态同步，使用 `SERIAL` 自增键管理步骤 Trace。
* **RedisStore**: 以 `task:{id}` 形式进行 Key-Value 存储，直接利用 JSON 格式序列化/反序列化整个任务对象，读写均为单次 $O(1)$ 操作，避免了传统关系型数据库的多表关联和复杂合并。

---

## ⚙️ 运行时切换配置指南 (Runtime Configuration Guide)

在启动服务时，可通过设置 `AI_AGENT_STORE_TYPE` 和 `AI_AGENT_STORE_DSN` 环境变量来随意切换不同的存储后端：

### 1. SQLite (默认方案)
```bash
export AI_AGENT_STORE_TYPE=sqlite
export AI_AGENT_STORE_DSN="data/agent.db"
go run ./cmd/server
```

### 2. Memory (内存版/无痕运行)
```bash
export AI_AGENT_STORE_TYPE=memory
go run ./cmd/server
```

### 3. PostgreSQL (分布式/高并发方案)
```bash
export AI_AGENT_STORE_TYPE=postgres
export AI_AGENT_STORE_DSN="postgres://postgres:password@127.0.0.1:5432/ai_agent?sslmode=disable"
go run ./cmd/server
```

### 4. Redis (高性能缓存/临时方案)
```bash
export AI_AGENT_STORE_TYPE=redis
export AI_AGENT_STORE_DSN="redis://:password@127.0.0.1:6379/0"
go run ./cmd/server
```

---

## 🧪 单元与集成测试校验 (Testing)

所有的存储介质在统一的测试标准下通过了契约测试：
```bash
go test -v ./internal/store
```

#### 运行结果输出：
```text
=== RUN   TestStores
    store_test.go:45: Skipping PostgresStore test: TEST_POSTGRES_DSN environment variable not set
    store_test.go:58: Skipping RedisStore test: TEST_REDIS_URL environment variable not set
=== RUN   TestStores/SQLiteStore
=== RUN   TestStores/MemoryStore
--- PASS: TestStores (0.02s)
    --- PASS: TestStores/SQLiteStore (0.01s)
    --- PASS: TestStores/MemoryStore (0.00s)
PASS
ok  	github.com/wuxujun/ai-agent/internal/store	1.655s
```
> **提示**: 如需在 CI/CD 中测试 Postgres 和 Redis，请分别设置 `TEST_POSTGRES_DSN` 和 `TEST_REDIS_URL` 环境变量，测试套件会自动将它们纳入执行流程。
