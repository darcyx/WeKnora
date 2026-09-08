# WeKnora 心智模型 — 当前实现

> 基于当前工作树（HEAD `174d20a1`）整理，更新于 2026-09-08；本次重点核对 Skills 配置、安装和执行链路。
> 本文是代码导航和修改边界说明，不替代部署文档、API 文档或配置参考。

## 一句话定位

WeKnora 是一个多租户 RAG/Agent 平台：Go 主服务负责 API、权限、知识库编排、文档分块、索引、检索、问答和异步任务；解析器、向量库、对象存储、模型、Neo4j、MCP 和沙箱都通过基础设施接口接入。

## 先记住这 7 件事

1. 根目录是 Go 服务；`cli/` 和 `client/` 是独立的 Go module，不能假设根目录的 `go test ./...` 会覆盖它们。
2. HTTP 入口是 Gin 路由，但“路由注册成功”不等于业务链路正确：DI、handler、service、repository 和运行时资源要一起追踪。
3. 文档解析与分块是两个边界：parser 返回 `ReadResult`（Markdown、图片引用、元数据），Go 端再按知识库配置分块和索引。
4. `Knowledge` 的“可检索”与“处理完成”不是同一时刻：文本索引完成后可以先启用，摘要、问题、图谱、Wiki 和多模态任务完成后才结束整个处理尝试。
5. 有 Redis 时使用 Asynq 多 worker pool；没有 `REDIS_ADDR` 时使用 `SyncTaskExecutor` 的 Lite 模式。业务代码依赖 `TaskEnqueuer` 接口，不应直接假设一定存在 Redis。
6. GraphRAG 在本项目中是“LLM 抽取 + Neo4j 一跳实体关系补召回”，不是带社区报告的全局 GraphRAG，也不是任意 N-hop 图遍历。
7. 生产对话的 Skills 来自当前沙箱配置的已安装技能；目录登记、安装完成、Agent 允许使用是不同层次。`@Skill` 只增加本轮优先使用提示，不收窄原有白名单。

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
| `TenantSkillCatalogEntity` / `TenantSkillEntity` / `TenantSandboxConfig` | 分别保存空间技能定义、某份沙箱配置的安装记录、后端参数与当前技能镜像指针 |
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
- MCP 有 service registry、OAuth token、tool approval gate 和 manager；当前通过 `discover_mcp_tools` 按需列举服务、工具和 schema，再用 `call_mcp_tool` 执行返回的 `tool_ref`。涉及工具执行的接口必须同时考虑租户、会话、审批和 sandbox trust boundary。
- `DataSourceService` 通过 `ConnectorRegistry` 校验凭据和资源，`Scheduler` 触发 `datasource:sync`，同步结果回到 Knowledge/Chunk 处理链；连接器不是另一套检索存储。
- Wiki 以页面/版本为源，经过 `wiki:ingest` 和 `wiki:finalize` 接入现有 chunk/search 体系，并有 Wiki-specific boost/上下文逻辑。
- `memory` service 管理用户主体级长期记忆：抽取任务异步化，检索结合 lexical/vector/topic 组织，支持确认、拒绝、清理和定期 consolidation。它与租户级聊天历史、KnowledgeBase 文档不是同一个数据源。

## Skills：从目录登记到本轮执行

### 三层对象和唯一生产来源

| 层次 | 职责 | 不代表什么 |
|---|---|---|
| 技能目录 `TenantSkillCatalogEntity` | 工作空间内登记技能名称、描述、来源和归档包，可分发到多份沙箱配置 | 目录里有记录不代表任何沙箱已经安装 |
| 安装记录 `TenantSkillEntity` | 绑定 `sandbox_config_id`，记录安装状态、启用开关、包版本、环境变量声明和快照信息 | `ready` 还需结合当前配置的镜像有效性判断 |
| Agent / 本轮 `AgentConfig` | 配置允许的名称集合，并注入本轮沙箱提供的 `TenantSkills` | 选择某个名称不会下载或自动安装技能 |

生产 QA 的 `configureSkillsFromAgent` 已不再填充 `SkillDirs`，也没有 `skills/preloaded/` 或 `WEKNORA_SKILLS_DIR` 回退。技能必须先安装到工作空间的具名沙箱配置（Docker、Cube 或 E2B），再由智能体选择该配置。`SkillDirs`、文件系统 `Loader` 和宿主资源 staging 仍保留给测试或显式构造运行时配置的调用者，不能把这些底层能力当成部署默认来源。仓库 `AGENTS.md` 约束开发助手，与产品内 Agent 的 Skills 选择无关。

### 配置、可用列表与本轮优先使用

配置入口是 [`AgentEditorModal.vue`](frontend/src/views/agent/AgentEditorModal.vue)，持久化字段在 [`CustomAgentConfig`](internal/types/custom_agent.go)。Skills 用于 `smart-reasoning` 模式：

| `skills_selection_mode` | 运行时结果 |
|---|---|
| `all` | `SkillsEnabled=true`，`AllowedSkills` 为空，允许本轮来源中的全部技能 |
| `selected` 且列表非空 | `SkillsEnabled=true`，`AllowedSkills=selected_skills`，按技能名称过滤 |
| `selected` 且列表为空 | 关闭 Skills |
| `none`、空值或未知模式 | 关闭 Skills |

前端只有已选沙箱，或空间里恰好有一份可选的具名沙箱配置时，才允许选 `all` / `selected`；后一种情况在启用 Skills 时自动绑定唯一配置。多份配置且未选定时需要先选沙箱。后端的模式转换本身不验证安装完成，实际可用性由下一层决定。

[`GET /api/v1/skills?sandbox_config_id=...`](docs/api/skill.md) 只返回该配置可用的安装技能，不再合并内置目录。不传配置时返回空数组和 `skills_available=false`；传入配置后的 `skills_available=true` 也不表示结果非空，应检查 `data`。前端 [`editorResources.ts`](frontend/src/stores/editorResources.ts) 使用此接口，技能目录管理和可执行技能列表是不同入口。

[`session_agent_qa.go`](internal/application/service/session_agent_qa.go) 组装本轮配置的关键链路：

```text
CustomAgent.Config
  ├─ configureSkillsFromAgent → SkillsEnabled / AllowedSkills / SandboxConfigID
  ├─ skillsForRun
  │    ├─ 优先采用会话现有沙箱绑定的配置，否则采用 Agent 选择
  │    └─ effectiveTenantSkills → 镜像有效 + ready + enabled → TenantSkills
  └─ applyPerRequestSkillScope(skill_names) → PinnedSkillNames
       保留请求顺序、去重、过滤白名单外的名称；不修改 AllowedSkills
```

[`tenant_skill_effective.go`](internal/application/service/tenant_skill_effective.go) 复用 `sandbox.SkillImageActive` 判断技能镜像是否可用；缺少配置、镜像不可用、读取失败或没有 `ready && enabled` 的记录时不提供技能。这里按调用上下文的工作空间读取，不直接使用共享 Agent 的所有者租户。已有沙箱的配置 pin 读取失败时，也不能猜测使用 Agent 当前配置。

`@Skill` / 请求 `skill_names` 现在是**本轮优先使用提示**：在原本允许的范围内设置 `PinnedSkillNames`，最终由 `resolvePinnedSkillInfos` 解析实际存在的技能并生成 `<must_use>` 提示。它既不撤销其他已允许技能，也不能越权启用被关闭或白名单外的技能。例如 Agent 允许 `[pdf, spreadsheet]`，本轮提及 `pdf` 后仍可使用两者，只优先提示 `pdf`。不要因函数名含 `Scope` 就把它描述成集合收窄。

### 登记、安装、卸载与镜像更新

包解析以 [`skill.go`](internal/agent/skills/skill.go) 为准：安装名称优先使用合法 `name`，否则采用合法 `slug`，再尝试从标题生成名称；允许 Unicode 字母、数字、连字符和下划线，名称最多 64 字符、描述最多 1024 字符。`skill_frontmatter.go` 可修复部分第三方 YAML 格式并记录修复标记，不改写原始 `SKILL.md`。因此 `selected_skills` 应使用接口返回的规范化名称。

[`tenant_skill_catalog.go`](internal/application/service/tenant_skill_catalog.go) 接收 ZIP 或公开来源，规范化包后写入目录；`InstallCatalogToConfigs` 再逐配置调用 `InstallSkill`，返回每个配置的安装 ID 或错误，允许部分受理成功。登记同名新包不会让旧安装自动升级。

[`tenant_skill_install.go`](internal/application/service/tenant_skill_install.go) 的主要流程如下：

```text
受理安装 → 安装记录 installing → 按 sandbox config 加锁
  → 从当前有效镜像启动维护沙箱
  → 清理目标技能目录、由服务端写入包文件
  → builtin-skill-installer 安装依赖
  → verifySkill 检查，可修复失败反馈给安装 Agent 再尝试
  → 写 manifest / 环境变量声明、清理临时工作区
  → 先记快照账本，再创建快照
  → 校验后端归属指纹并切换 SkillImage 指针
  → 安装记录 ready、处理旧快照与现有会话更新
```

安装 Agent 负责准备依赖，不负责从提示词重建包文件；其自然语言回复不构成安装成功，验证与镜像指针切换才是关键边界。安装模式通过专用运行时开关开放 `shell_exec`、`write_skill_file` / `edit_skill_file`，普通自定义 Agent 不能仅靠配置字段进入该模式。安装进度和对话记录分别由 `tenant_skill_progress.go`、`tenant_skill_transcript.go` 提供；前端 `SandboxSkillsPanel.vue` 用 SSE 跟踪进度并以轮询刷新状态。

卸载走 [`tenant_skill_remove.go`](internal/application/service/tenant_skill_remove.go)：`removing → removed`，删除镜像中的技能文件并生成新快照；失败记录为 `failed`。安装和卸载共用配置锁，避免两个操作分别从旧镜像派生、覆盖彼此结果。启用开关只控制后续可用列表，不等同于卸载。停止、失败恢复和孤儿快照回收分别见 `tenant_skill_stop.go`、`tenant_skill_reaper.go`。

镜像发布策略由 `TenantSandboxConfig.skill_rollout` 决定：默认 `next_turn` 将已有 binding 标记 stale，在后续轮次首次解析时重建沙箱，避免同一轮中途切换；`new_session` 保留已有沙箱，只让新沙箱使用新镜像。标记 stale 是镜像切换后的最佳努力操作。因此配置当前镜像、技能记录和某个已运行沙箱的实际内容仍需分别检查，不能承诺 `ready` 后所有活会话立即更新；重建还涉及会话工作区生命周期。

### 渐进披露、资源读取与执行环境

[`agent_service.go`](internal/application/service/agent_service.go) 仅在 `SkillsEnabled` 且存在 `TenantSkills` 或显式 `SkillDirs` 时初始化 `skills.Manager`；Manager 按 `AllowedSkills` 过滤来源，向提示词提供名称和描述。渐进披露指给模型的内容按需展开，不应解释为所有实现都只在磁盘读取 frontmatter。

| 步骤 | 工具与职责 |
|---|---|
| 发现 | System Prompt 中的可用技能元数据，配合本轮优先使用提示 |
| 读说明 | `read_file(path="skill://pdf/SKILL.md")`，返回指令、资源和执行方式 |
| 读附加资源 | `read_file(path="skill://pdf/references/forms.md")`，支持分页和输出预算 |
| 执行 | `shell_exec(skill_name="pdf", command=...)`，准备对应技能环境后在会话沙箱中运行 |

租户来源 [`tenant_source.go`](internal/agent/skills/tenant_source.go) 从安装记录和归档包提供技能资源，无需为了阅读启动沙箱；执行副本则在沙箱镜像里。`loadInstalledSkillBundle` 优先读安装记录的 `BundleRef`，旧记录回退目录包时检查 `BundleSHA256`，避免目录已更新却把新文档配给旧镜像执行。`skill://` 是资源地址，不能作为 Shell 路径。

```json
{
  "skill_name": "pdf",
  "command": "python3 \"$WEKNORA_SKILL_DIR/scripts/extract.py\" /workspace/input/report.pdf"
}
```

`shell_exec` 的技能环境只作用于本次调用；工作目录默认 `/workspace`，输入附件在 `/workspace/input`，产物放在 `/workspace/output`。普通 Shell 的注册取决于 `SkillsEnabled` 和后端 Shell 能力，不要求已经存在 ready 技能；Skills 关闭时，文件工具仍可按沙箱能力注册，用于附件和产物操作。独立的 `read_skill`、`read_sandbox_file`、`execute_skill_script` 已退出当前工具注册链，旧名称只用于历史记录兼容。

凭据由 [`user_env_resolver.go`](internal/application/service/user_env_resolver.go) 按顺序覆盖：管理员的技能共享值 → 当前调用者的配置级变量 → 当前调用者的技能级变量。调用者身份使用上下文 `Principal`，不能用同一工作空间的公共 IM 账号代替；读取个人变量失败会返回错误，不静默降级使用管理员凭据。缺依赖时可在会话内补装，修改随该会话沙箱销毁，不会更新其他会话的基础镜像；要长期生效仍需重新安装技能。

### 修改与排查顺序

1. 先区分目录定义、某配置的安装记录和运行中会话；检查 `sandbox_config_id`、会话 pin、镜像有效性、安装状态和开关。
2. 再检查 Agent 模式、`skills_selection_mode` / `selected_skills`、本轮 `PinnedSkillNames`；名称以解析后的安装名称为准，不按展示标题或目录名猜测。
3. 读失败追踪 Manager 白名单、包引用与摘要校验、资源路径；执行失败追踪 Shell 能力、技能环境、调用者变量和实际镜像版本。
4. 安装失败查看安装 transcript、验证结果、快照账本及镜像指针；HTTP 已受理、SSE 已结束和镜像已发布不能混用。

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
| 新增或调整 Agent Skill | 目录/安装/镜像链 → `effectiveTenantSkills` / 会话 pin → `configureSkillsFromAgent` → Manager 白名单 → 本轮 pin 提示 → `read_file` / `shell_exec` |

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
- Agent/Skills：[`internal/application/service/session_agent_qa.go`](internal/application/service/session_agent_qa.go)、[`internal/application/service/tenant_skill_effective.go`](internal/application/service/tenant_skill_effective.go)、[`internal/application/service/tenant_skill_catalog.go`](internal/application/service/tenant_skill_catalog.go)、[`internal/application/service/tenant_skill_install.go`](internal/application/service/tenant_skill_install.go)、[`internal/agent/skills/manager.go`](internal/agent/skills/manager.go)、[`internal/application/service/tenant_skill_admin.go`](internal/application/service/tenant_skill_admin.go)、[`internal/handler/skill_handler.go`](internal/handler/skill_handler.go)、[`docs/agent-skills.md`](docs/agent-skills.md)、[`docs/api/agent.md`](docs/api/agent.md)、[`docs/api/skill.md`](docs/api/skill.md)
- 资源与扩展：[`internal/application/service/file/`](internal/application/service/file)、[`datasource_service.go`](internal/application/service/datasource_service.go)、[`wiki_ingest.go`](internal/application/service/wiki_ingest.go)、[`agent_service.go`](internal/application/service/agent_service.go)、[`memory/`](internal/application/service/memory)
- 客户端：[`frontend/`](frontend)、[`cli/`](cli)、[`client/`](client)、[`mcp-server/`](mcp-server)、[`miniprogram/`](miniprogram)

## 验证入口

- Skills 重点回归入口：`session_agent_qa_scope_test.go`（模式与本轮 pin）、`tenant_skill_effective_test.go`（可用来源）、`tenant_skill_install_test.go` / `tenant_skill_remove_test.go`（镜像变更）、`agent_service_skill_bundle_test.go`（资源版本）、`user_env_resolver_test.go`（凭据覆盖）、`internal/agent/skills/` 与 `internal/agent/tools/` 的相关测试。
- 根 Go 服务：在仓库根目录运行针对改动包的 `go test`；必要时为受限环境指定任务专用 `GOCACHE/GOMODCACHE`。
- CLI：在 [`cli/`](cli) 内运行 `go test ./...`；不要从根 module 推断 CLI 已验证。
- SDK：在 [`client/`](client) 内运行 `go test ./...`。
- Python parser：在 [`docreader/`](docreader) 内运行对应 pytest/parser tests。
- 前端：按 `frontend/package.json` 的脚本执行 type-check/build；这些检查不能代替真实浏览器、HTTP API、SSE 和权限链路验收。
