# WeKnora 心智模型 — 当前实现

> 基于当前工作树（HEAD `7c7d5da3`）整理，更新于 2026-08-25。
> 本文是代码导航和修改边界说明，不替代部署文档、API 文档或配置参考。

## 一句话定位

WeKnora 是一个多租户 RAG/Agent 平台：Go 主服务负责 API、权限、知识库编排、文档分块、索引、检索、问答和异步任务；解析器、向量库、对象存储、模型、Neo4j、MCP 和沙箱都通过基础设施接口接入。

## 先记住这 6 件事

1. 根目录是 Go 服务；`cli/` 和 `client/` 是独立的 Go module，不能假设根目录的 `go test ./...` 会覆盖它们。
2. HTTP 入口是 Gin 路由，但“路由注册成功”不等于业务链路正确：DI、handler、service、repository 和运行时资源要一起追踪。
3. 文档解析与分块是两个边界：parser 返回 `ReadResult`（Markdown、图片引用、元数据），Go 端再按知识库配置分块和索引。
4. `Knowledge` 的“可检索”与“处理完成”不是同一时刻：文本索引完成后可以先启用，摘要、问题、图谱、Wiki 和多模态任务完成后才结束整个处理尝试。
5. 有 Redis 时使用 Asynq 多 worker pool；没有 `REDIS_ADDR` 时使用 `SyncTaskExecutor` 的 Lite 模式。业务代码依赖 `TaskEnqueuer` 接口，不应直接假设一定存在 Redis。
6. GraphRAG 在本项目中是“LLM 抽取 + Neo4j 一跳实体关系补召回”，不是带社区报告的全局 GraphRAG，也不是任意 N-hop 图遍历。

## 总体架构

```text
前端 / CLI / Go SDK / MCP Server / 小程序 / Desktop / Embed / IM
                              │
                              ▼
Gin HTTP API（/api/v1） + Embed/IM 公共入口
                              │
       认证、API Key capability、RBAC、租户/资源归属校验
                              │
                              ▼
handlers → application services → repositories / domain interfaces
                              │
       ┌──────────────────────┼──────────────────────┐
       ▼                      ▼                      ▼
关系库/GORM              Redis/Asynq             外部基础设施
元数据、chunks、任务状态    异步任务/流状态           parser、模型、向量库、对象存储
                                                  Neo4j、DuckDB、MCP、沙箱
```

### 启动与依赖注入

主流程在 [`cmd/server/main.go`](cmd/server/main.go)：

```text
main
 └─ container.BuildContainer(runtime.GetContainer())
     ├─ config.LoadConfig / initDatabase / initFileService / initRedisClient
     ├─ retrieve engine registry、DocReader、Neo4j、DuckDB、StreamManager
     ├─ repositories
     ├─ application services
     ├─ chat pipeline plugins、Agent/MCP/Memory/DataSource/Wiki
     ├─ handlers
     └─ router.NewRouter + 路由注册
```

`internal/container/container.go` 是实际 DI 清单。运行时主关系库目前由 `DB_DRIVER` 选择 PostgreSQL 或 SQLite，并通过 `internal/database` 执行对应迁移；SQLite 还注册本地向量能力。`initFileService`、`engine_factory.go` 和各类 registry 决定外部资源的实际实现。

服务启动后由 `main` 创建 HTTP server，监听失败会重试；收到退出信号时先释放 listener，再执行 HTTP graceful shutdown 和 `ResourceCleaner`。后台 scheduler、housekeeping、audit retention、temporary-document cleanup 等生命周期组件也由容器启动。

## 访问与多租户边界

- 普通 API 位于 `/api/v1`；认证支持 JWT 和 API Key。API Key 还带有 chat、retrieve、ingest、manage 等 capability，路由通过 `rbacGuards` 和 API-key gate 组合校验。
- handler/service 从认证上下文解析 tenant、user、session owner；不要信任请求体中的 tenant 或 owner 字段。
- 同租户资源和组织共享资源走不同授权路径。`HybridSearch` 会对请求中的每个 KB 显式检查访问权限，并在 VectorStore 解析时再次验证 store owner，避免仅凭 UUID 访问其他租户的索引。
- 前端路由和菜单权限只是 UX 层；真正的授权在 middleware、handler/service 的资源归属检查和 API-key capability gate 中。
- Embed、IM callback 和公开聊天入口有独立的 token/session 认证链路，不应复用普通后台用户会话的假设。

## 核心数据模型

| 抽象 | 代码中的职责 |
|---|---|
| `Tenant` / `Organization` | 工作空间、成员角色、邀请、组织共享和审计边界 |
| `KnowledgeBase` | 检索范围及其模型、分块、FAQ/Wiki/图谱/索引策略配置；可绑定 VectorStore |
| `Knowledge` | 一个入库对象（文件、URL、段落、手工内容、数据源同步结果或 Wiki 页面），持有解析和后处理状态 |
| `Chunk` | 文本、父文本、摘要、问题、FAQ、图片 OCR/Caption、表格摘要、实体关系、Wiki 等检索/上下文单元 |
| `VectorStore` | 租户可管理的向量/关键词引擎实例；凭据在持久化时加密，运行时按 store ID 动态建引擎 |
| `StorageBackend` | 文件、图片、临时附件和资源的对象存储实例；与 VectorStore 是两条独立配置链 |
| `Session` / `Message` | 会话、消息、流式回答、引用、附件、建议和 Agent 产物 |
| `DataSource` / `WikiPage` | 外部连接器同步和 Wiki 页面版本化内容，最终可进入现有 Knowledge/Chunk/检索链 |
| `CustomAgent` / `MCPService` | Agent 提示词、检索策略、工具、MCP OAuth/审批、Skills 选择和沙箱策略 |
| `TenantSkill` / `SandboxConfig` | 工作空间沙箱中安装、启用并随镜像提供给 Agent 的技能 |
| `MemorySubject` / `MemoryItem` | 按租户和用户主体隔离的长期记忆、主题、向量、确认/拒绝和清理状态 |

`Knowledge.ParseStatus` 的关键状态是 `pending → processing → finalizing → completed`，也可能进入 `failed`、`cancelled` 或 `deleting`；`SummaryStatus` 单独表示摘要任务状态，不能用它替代整体解析状态。

## 文档入库链路

### 入口与主任务

[`internal/application/service/knowledge_create.go`](internal/application/service/knowledge_create.go) 接收文件、URL、`file_url`、段落和手工编辑等来源，创建 `Knowledge` 记录并投递 `document:process`。任务 payload 会携带 tenant、KB、knowledge、文件信息、语言、功能开关和本次 `Attempt`，worker 不能依赖原始 HTTP context 仍然存在。

```text
handler.Knowledge
  └─ KnowledgeService.CreateKnowledge...
      └─ Knowledge(pending) + document:process
          └─ KnowledgeService.ProcessDocument
```

`ProcessDocument` 的固定检查顺序很重要：重新加载 tenant/knowledge/KB → 跳过已完成、取消或删除中的记录 → 在写入 `processing` 前再次检查 abort → 解析、分块、索引和后处理。URL/file_url 路径在下载前后都有 SSRF/文件类型校验；视频当前明确不支持，音频需要 ASR 配置，图片需要多模态能力。

### 解析器边界

Go 的 [`internal/infrastructure/docparser/`](internal/infrastructure/docparser/) 通过 engine registry 统一返回 `interfaces.DocReader`。当前引擎包括：

- `builtin`：Python `docreader` 服务，复杂格式通过 gRPC；支持 Doc/Docx、PDF、Excel、PPT、Markdown、EPUB、HTML/MHTML、XMind、图片和音频等路径。
- `simple`：Go 进程内处理 txt/Markdown/CSV/JSON、图片和音频等简单格式。
- `anydoc`：进程内 office 文档转换，是否可用取决于构建绑定。
- `weknoracloud`、`mineru`、`mineru_cloud`、`paddleocr_vl`、`paddleocr_vl_cloud`：远程或自托管解析服务，凭据/endpoint 从租户配置和 engine override 解析。

Python [`docreader/main.py`](docreader/main.py) 暴露 `Read`、`ReadStream`、`ListEngines`；[`docreader/parser/registry.py`](docreader/parser/registry.py) 决定文件类型到 parser 的映射，并在指定引擎不支持时回退到 builtin。所有 reader 最终都产出统一的 Markdown、图片引用和 metadata，后续图片落存储、表格规范化和分块不应塞进某个 parser 的私有路径。

### Go 端分块、存库和索引

[`internal/application/service/knowledge_process.go`](internal/application/service/knowledge_process.go) 的 `processChunks` 是入库核心：

1. 检查取消/删除状态，解析 KB 的 `ChunkingConfig`，交给 `internal/infrastructure/chunker/` 做 Go 端分块。父子分块由 `SplitParentChild`/派生配置生成；普通分块保留顺序和 `prev/next`，子块通过 `ParentChunkID` 指向上下文父块。
2. 先清理同一 knowledge 的旧 chunks、索引和 Neo4j 图数据，保证重解析幂等。
3. chunks 总是写入关系库，即使当前 KB 没有向量/关键词索引；Wiki、图谱、摘要、父块上下文仍依赖这些记录。
4. 只有可检索的文本/子块进入 `IndexInfo`；父块只入库、不做向量 embedding。索引内容会合并标题/`ContextHeader`/文档级 metadata，再由 `RetrieveEngine.BatchIndex` 写入向量和/或关键词索引。
5. 图片解析结果经过 image resolver 写入 StorageBackend，并生成 OCR/Caption 派生 chunk；多模态任务可在主文本索引后异步执行。
6. `finalizeIndexedKnowledgeState` 先把 `EnableStatus` 设为 enabled。若仍有文本或多模态/后处理工作，`ParseStatus` 保持 processing；没有任何 enrichment 时才可直接 completed。

## 后处理与任务状态机

`KnowledgePostProcessService.Handle` 是一次入库尝试从“主索引”交接到“增强任务”的唯一协调点：

```text
processing
   │ SetFinalizing(pending_subtasks)
   ├─ summary:generation       → Summary chunk + index
   ├─ question:generation[n]   → 每批最多 20 个 text chunks
   ├─ chunk:extract[n]         → 可选 Graph RAG 实体/关系抽取
   ├─ image:multimodal[n]      → OCR / VLM caption 派生内容
   ├─ wiki:ingest               → Wiki KB 级防抖批处理（若启用）
   ├─ knowledge:auto_tag        → 最佳努力的自动标签，不占完成计数
                                  │
                                  └─ 每个拥有 slot 的任务 FinalizeSubtask
                                      counter=0 → completed
```

摘要、问题、图谱和 Wiki 任务会带 `KnowledgeID`/`Attempt`，使重试落在同一次追踪和状态尝试上。投递失败的 slot 必须由 post-process 释放，否则记录会永久停在 `finalizing`；取消/删除和并发重复投递也必须在 worker 内再次检查。后处理完成不代表每个增强结果都成功：主文档可完成，某个非关键 enrichment 可单独失败并记录日志/状态。

## 异步执行拓扑

任务声明集中在 [`internal/types/task.go`](internal/types/task.go)，队列到 worker pool 的映射也在此维护，producer 通过 task type/queue 投递：

| Pool | 队列 | 代表任务 |
|---|---|---|
| `core` | `default`, `chat_attachment` | 文档/手工处理、会话临时附件 |
| `postprocess` | `postprocess` | `knowledge:post_process` |
| `enrichment` | `summary`, `multimodal`, `graph`, `question`, `memory` | 摘要、图片、图谱、问题、长期记忆 |
| `maintenance` | `sync`, `low` | 数据源同步、FAQ 导入、KB/索引/Knowledge 维护 |
| `wiki` | `wiki` | Wiki ingest/finalize |
| `shared` | 弹性订阅 core/enrichment 的部分队列 | 借用空闲容量，不取代 dedicated pool 的最低保障 |

[`internal/router/task.go`](internal/router/task.go) 将 task handler 绑定到各个 Asynq server；`internal/router/sync_task.go` 在 Lite 模式下以内存执行任务。不要只修改 `types/task.go` 的常量：新增任务还需要 payload、enqueue helper、handler 注册、DI 和必要的取消/重试/死信处理。

Wiki 另有持久化的 `task_pending_ops`：每个 Knowledge 先写 pending op，再投递 KB 级防抖 trigger；worker 按 KB 加锁、批量消费并在剩余时继续调度。这样 Wiki 的 durable queue 不依赖 Redis，Lite 模式也能工作。

## 检索与索引运行时

### Engine registry 与 VectorStore

`internal/application/service/retriever/` 提供 `RetrieveEngineService`、`CompositeRetrieveEngine`、`RetrieveEngineRegistry` 和按 KB/store 的 factory：

- 环境路径可注册 PostgreSQL/SQLite；动态 VectorStore factory 当前覆盖 PostgreSQL、SQLite、Elasticsearch v7/v8、OpenSearch、Qdrant、Milvus、Weaviate、Doris 和 Tencent VectorDB 等实现。
- Registry 同时维护按 engine type 和按 store ID 的映射；store miss 时可从数据库重新加载并构建，失败有并发合并/冷却保护。
- KB 到 VectorStore 的绑定不是凭字符串直接拼接连接：factory 会校验 endpoint、租户归属和允许的 engine 类型。VectorStore 和 StorageBackend 的“平台配置”不能互相替代。

### `HybridSearch`

[`internal/application/service/knowledgebase_search.go`](internal/application/service/knowledgebase_search.go) 的实际顺序是：

1. 规范化 `match_count`，解析多 KB scope，并逐个做共享 KB/租户授权。
2. 检查同一搜索范围的 embedding model 一致性，主 KB 决定 query embedding 和 FAQ 类型；query embedding 尽量只计算一次。
3. 按 `(store ID, owner tenant)` 分组，解析每组 engine，并行执行 vector/keyword retrieval；多 store 结果先做 engine-aware score normalization。
4. 分类 vector 与 keyword hits，按租户检索配置进行融合/去重；FAQ 再做优先级和迭代 TopK 等特定处理。
5. 结果回到 chat pipeline，继续 parent context、邻近 chunk、图片信息、FAQ answer 和 rerank 等处理。

索引端使用 `KeywordsVectorHybridRetrieveEngine`：是否生成 embedding、是否写 keyword/vector，由 KB 的 retriever types 和 engine capabilities 决定。不要把“关系库存有 chunks”误判成“该 KB 已经能向量检索”。

## 问答流水线

[`internal/application/service/session_knowledge_qa.go`](internal/application/service/session_knowledge_qa.go) 先解析 KB/Knowledge/tag scope、模型、Web Search、Agent override、历史和长期记忆，再动态选择纯聊天或 RAG pipeline：

```text
纯聊天：LOAD_HISTORY? → MEMORY_RECALL → CHAT_COMPLETION_STREAM

RAG：LOAD_HISTORY? → MEMORY_RECALL → QUERY_UNDERSTAND
   → CHUNK_SEARCH_PARALLEL → CHUNK_RERANK → WEB_FETCH?
   → CHUNK_MERGE → FILTER_TOP_K → DATA_ANALYSIS?
   → INTO_CHAT_MESSAGE → CHAT_COMPLETION_STREAM
```

流水线由 `chat_pipeline.EventManager` 以插件链实现，而不是在一个巨型 handler 中硬编码：

- `QueryUnderstand`：重写/扩展问题，识别意图、图片描述和查询实体；无需检索时可跳过 RAG。
- `SearchParallel`：并行执行普通 chunk search 和可选 Neo4j entity search。
- `Rerank`：调用租户配置的 reranker，结合阈值、TopK、FAQ 优先级和 MMR 等策略。
- `Merge`：去重、注入历史引用、child→parent 上下文恢复、按文档/类型合并、FAQ answer 填充、邻近 chunk 扩展，再次去重。
- `WebFetch`、`DataAnalysis`：分别补充网页内容或通过 DuckDB 处理表格/数据分析结果。
- `IntoChatMessage` + completion：组装上下文和 citations，支持普通/流式模型输出；引用在流式回答前通过 SSE event 发出，停止请求通过 StreamManager 取消正在执行的 pipeline。

无结果或检索失败会进入配置的 fixed/model fallback；纯聊天、RAG、fallback 和 stop 是不同路径，不能用“最终 HTTP 200”判断检索一定成功。

## Graph RAG 的实际边界

入库阶段的 graph extract worker 使用 LLM 从 text chunks 抽取实体和关系，写入 Neo4j；重解析会按 knowledge namespace 删除旧图。查询阶段先从 QueryUnderstand 得到实体文本，再调用 Neo4j `SearchNode`：匹配实体名称并返回与其直接相连的关系节点，再根据节点携带的 chunk IDs 补充普通搜索结果，标记为 `MatchTypeGraph`。

因此当前实现擅长“围绕指定实体补回相关文档块”，但没有代码证据表明它实现了社区发现、全局 report、PageRank、图 embedding 或通用多跳路径评分。修改或写文档时应保持这个边界。

## Agent、MCP、数据源和长期记忆

- `AgentService` 是独立于 `KnowledgeQA` 的 Agent 执行路径，组合知识检索、Web Search、Wiki、DuckDB、MCP tools、工具审批、tenant sandbox、skills 和 artifact collector；`AgentEngine` 每轮由 session 传入上下文，不应假设引擎自身持久化整段会话。
- MCP 有 service registry、OAuth token、tool approval gate 和 manager；涉及工具执行的接口必须同时考虑租户、会话、审批和 sandbox trust boundary。
- Skills 的“哪些生效”在 WeKnora 的 `CustomAgent.Config` 中配置，不是仓库根目录的 `AGENTS.md`：前端 [`AgentEditorModal.vue`](frontend/src/views/agent/AgentEditorModal.vue) 的 Agent 模式 Skills 区域维护 `skills_selection_mode` 和 `selected_skills`，API 字段定义见 [`frontend/src/api/agent/index.ts`](frontend/src/api/agent/index.ts)，后端模型见 [`internal/types/custom_agent.go`](internal/types/custom_agent.go)。
- `skills_selection_mode` 有三个值：`all` 启用全部可用 Skills，`selected` 只允许 `selected_skills` 中的名称，`none` 或空值关闭 Skills；该配置只对 `smart-reasoning` Agent 有效。`selected` 但列表为空、未知模式都会按关闭处理。
- 一次 Agent 会话由 [`session_agent_qa.go`](internal/application/service/session_agent_qa.go) 把上述配置转换为运行时 `AgentConfig.SkillsEnabled`、`SkillDirs` 和 `AllowedSkills`。因此修改 API/数据库字段时，要同时检查这段转换，不要只改前端 checkbox。
- Skill 有两类来源：服务端 [`skills/preloaded/`](skills/preloaded/)（可由 `WEKNORA_SKILLS_DIR` 覆盖）是内置 Skill 目录；工作空间沙盒则提供上传到镜像中的租户 Skill。后者由 `sandbox_config_id` 决定运行在哪个沙盒，且只有镜像有效、状态为 `ready`、开关为 `enabled` 时才会注入本次会话。
- [`GET /api/v1/skills?sandbox_config_id=...`](docs/api/skill.md) 返回服务端 `skills/preloaded/` 的内置 Skill；传入沙盒配置时再合并该沙盒中可执行的租户 Skill，同名租户 Skill 覆盖内置 Skill。前端的 Skill 选择列表通过 [`editorResources.ts`](frontend/src/stores/editorResources.ts) 调这个接口，即使未选择沙盒也能看到内置 Skill。
- 本地沙盒不会显示“上传 Skill”步骤，这是前端 [`SandboxConfigEditorDrawer.vue`](frontend/src/components/SandboxConfigEditorDrawer.vue) 的明确分支：Skill 镜像安装只对远程沙盒开放。使用内置 Skill 时，把目录放在 `skills/preloaded/`，或设置 `WEKNORA_SKILLS_DIR` 指向该目录；为避免工作目录/二进制目录不同导致找不到，部署时优先显式设置这个环境变量。
- 本地内置 Skill 的最小 Agent 配置是：`agent_mode=smart-reasoning`、选择本地 `sandbox_config_id`（需要执行 Skill 脚本时）、`skills_selection_mode=all`。若用 `selected`，`selected_skills` 填的是每个 `SKILL.md` frontmatter 的 `name`，不是目录名；例如当前内置目录中的 `数据处理器`、`引用生成器`。
- 配置层和运行时来源是两道门：内置 Skill 由 `SkillDirs`/`AllowedSkills` 控制，租户 Skill 还必须存在于当前会话实际启动的沙盒镜像；对话中的 `@Skill` / `skill_names` 会在本轮把已允许的白名单进一步收窄，不能越过 Agent 配置的限制。相关 scope 逻辑在 `applyPerRequestSkillScope`。
- `DataSourceService` 通过 `ConnectorRegistry` 校验凭据和资源，`Scheduler` 触发 `datasource:sync`，同步结果回到 Knowledge/Chunk 处理链；连接器不是另一套检索存储。
- Wiki 以页面/版本为源，经过 `wiki:ingest` 和 `wiki:finalize` 接入现有 chunk/search 体系，并有 Wiki-specific boost/上下文逻辑。
- `memory` service 管理用户主体级长期记忆：抽取任务异步化，检索结合 lexical/vector/topic 组织，支持确认、拒绝、清理和定期 consolidation。它与租户级聊天历史、KnowledgeBase 文档不是同一个数据源。

## 文件与资源存储

`internal/application/service/file/` 通过 `FileService` 和 `StorageBackendResolver` 统一本地文件、图片、临时附件、资源 URL 和下载安全。当前 provider 包括 local、MinIO、COS、TOS、S3、OBS、OSS、KS3；KB 可以绑定租户级 backend，密钥在数据库 JSON 中加密，API response 会脱敏。

文件路径、presigned URL、资源下载和 parser 的图片引用都必须经过 allowlist/SSRF/path traversal 防护。不要把客户端传入的 storage URL 直接交给浏览器或解析器，也不要把 `StorageBackend` 与向量检索 engine 混为一谈。

## 修改时的扩展入口

| 需求 | 需要同步检查的代码路径 |
|---|---|
| 新 API 功能 | `internal/types` → repository interface/实现 → service → handler → `internal/router/routes_*.go` → `internal/container/container.go` |
| 新异步任务 | task type/payload → enqueue helper → `internal/router/task.go` handler → queue definition/重试/死信 → DI 和状态计数 |
| 新解析引擎 | `internal/infrastructure/docparser/engines.go`、engine registry、reader；Python parser 另看 `docreader/parser/registry.py` |
| 新向量/关键词引擎 | `internal/types/retriever.go`、repository、`engine_factory.go`、registry、VectorStore 校验和迁移 |
| 新文件存储 provider | `FileService`、provider 实现、factory、`storageallowlist`、配置/脱敏/安全测试 |
| 新聊天阶段 | `EventType`、plugin 的 `ActivationEvents/OnEvent`、pipeline builder、`BuildContainer` 注册和阶段测试 |
| 新数据源 | connector registry、config 校验、scheduler、sync task、Knowledge 幂等更新 |
| 新增或调整 Agent Skill | `skills/preloaded/` 或沙盒技能安装链 → `SkillHandler`/`TenantSkillService` → `CustomAgentConfig` 的 `skills_selection_mode`/`selected_skills` → `configureSkillsFromAgent` → 本轮 `@Skill` scope |

## 不要破坏的架构约束

1. 保持 `handler → service → repository/infrastructure` 分层；不要在 handler 里直接写 GORM、调用向量库或拼接外部凭据。
2. 解析器只负责把输入变成统一 `ReadResult`；分块、父子关系、索引和 Knowledge 状态由 Go application service 负责。
3. 异步 worker 必须重新加载租户和资源，并在重型操作前复查 cancelled/deleting；重解析必须先清理旧 chunks/index/graph。
4. `processing → finalizing → completed` 的 pending-subtask 计数是状态机契约；新增后处理任务必须明确是否占 slot，以及投递失败如何释放 slot。
5. 跨租户共享检索必须同时通过资源授权和 VectorStore ownership 检查；前端隐藏按钮不能替代后端授权。
6. 新增/修改 DI 后优先更新构造器和 `BuildContainer`，不要把运行时单例或生成结果藏在 handler 全局变量里。
7. 需要确认“能否运行”时，分别验证构建、数据库迁移、外部服务连接、任务消费者和真实 HTTP/SSE；Go 编译通过不等于浏览器、Redis worker 或模型调用已经验证。

## 重要导航文件

- 启动与 DI：[`cmd/server/main.go`](cmd/server/main.go)、[`internal/container/container.go`](internal/container/container.go)
- HTTP 路由：[`internal/router/router.go`](internal/router/router.go)、[`internal/router/routes_*.go`](internal/router)
- 任务拓扑：[`internal/types/task.go`](internal/types/task.go)、[`internal/router/task.go`](internal/router/task.go)、[`internal/router/sync_task.go`](internal/router/sync_task.go)
- 入库主链：[`internal/application/service/knowledge_create.go`](internal/application/service/knowledge_create.go)、[`knowledge_process.go`](internal/application/service/knowledge_process.go)、[`knowledge_post_process.go`](internal/application/service/knowledge_post_process.go)
- 解析：[`internal/infrastructure/docparser/`](internal/infrastructure/docparser)、[`docreader/main.py`](docreader/main.py)、[`docreader/parser/`](docreader/parser)
- 检索：[`internal/application/service/knowledgebase_search.go`](internal/application/service/knowledgebase_search.go)、[`internal/application/service/retriever/`](internal/application/service/retriever)、[`internal/container/engine_factory.go`](internal/container/engine_factory.go)
- 聊天：[`internal/application/service/session_knowledge_qa.go`](internal/application/service/session_knowledge_qa.go)、[`internal/application/service/chat_pipeline/`](internal/application/service/chat_pipeline)
- Agent/Skills：[`internal/application/service/session_agent_qa.go`](internal/application/service/session_agent_qa.go)、[`internal/application/service/skill_service.go`](internal/application/service/skill_service.go)、[`internal/application/service/tenant_skill_admin.go`](internal/application/service/tenant_skill_admin.go)、[`internal/handler/skill_handler.go`](internal/handler/skill_handler.go)、[`docs/agent-skills.md`](docs/agent-skills.md)、[`docs/api/agent.md`](docs/api/agent.md)、[`docs/api/skill.md`](docs/api/skill.md)
- 资源与扩展：[`internal/application/service/file/`](internal/application/service/file)、[`datasource_service.go`](internal/application/service/datasource_service.go)、[`wiki_ingest.go`](internal/application/service/wiki_ingest.go)、[`agent_service.go`](internal/application/service/agent_service.go)、[`memory/`](internal/application/service/memory)
- 客户端：[`frontend/`](frontend)、[`cli/`](cli)、[`client/`](client)、[`mcp-server/`](mcp-server)、[`miniprogram/`](miniprogram)

## 验证入口

- 根 Go 服务：在仓库根目录运行针对改动包的 `go test`；必要时为受限环境指定任务专用 `GOCACHE/GOMODCACHE`。
- CLI：在 [`cli/`](cli) 内运行 `go test ./...`；不要从根 module 推断 CLI 已验证。
- SDK：在 [`client/`](client) 内运行 `go test ./...`。
- Python parser：在 [`docreader/`](docreader) 内运行对应 pytest/parser tests。
- 前端：按 `frontend/package.json` 的脚本执行 type-check/build；这些检查不能代替真实浏览器、HTTP API、SSE 和权限链路验收。
