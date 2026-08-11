# WeKnora 心智模型 — 知识库文档处理链路

> 由 `/codebase-mastery` skill 生成 · 2026-07-15

## 一句话定位

WeKnora（腾讯开源）是基于 LLM 的文档理解与语义检索框架（RAG）：Go 主服务负责编排、分块、索引与问答，Python `docreader` 微服务负责多格式文档解析。

## 技术架构（文档处理视角）

```
前端/API (internal/handler/knowledge.go)
   │  上传文件 / URL / 文本段落
   ▼
knowledge_create.go  ──创建 Knowledge 记录(ParseStatus=pending)──► PostgreSQL
   │  asynq.NewTask("document:process") 入队 Redis
   ▼
Asynq Worker: ProcessDocument (knowledge_process.go:2683)
   │  幂等检查 → 状态置 processing
   ├─ convert()  ──gRPC──►  docreader (Python, 按格式选 parser → Markdown)
   ├─ ASR 转写（音频）
   ├─ imageResolver: 图片落存储、改写 Markdown 引用
   ├─ chunker.Split / SplitParentChild（Go 端分块）
   ▼
processChunks (knowledge_process.go:259)
   │  清理旧 chunks/索引/图谱（幂等）
   ├─ chunkService.CreateChunks → PostgreSQL
   ├─ retrieveEngine.BatchIndex（embedding 向量 + 关键词索引）
   ├─ finalizeIndexedKnowledgeState：文本索引完成即可检索
   └─ 入队后处理任务
        ▼
KnowledgePostProcessService.Handle (knowledge_post_process.go:58)
   ├─ TypeSummaryGeneration   摘要生成（LLM）→ Summary chunk 入库+索引
   ├─ TypeQuestionGeneration  按 chunk 批次生成问题 → 索引
   ├─ Graph RAG extract       每个文本 chunk 一个图谱抽取任务（可选）
   └─ 多模态图片任务          图片 Caption/OCR → 派生 chunk → 索引
```

## 技术栈

- **主服务**：Go（Gin 路由 / GORM / asynq 任务队列 + Redis）
- **文档解析**：Python gRPC 微服务 `docreader/`（uv 管理依赖）
- **存储**：PostgreSQL（元数据+chunks）、pgvector / Elasticsearch（向量与关键词检索）、Neo4j（可选图谱）、MinIO/COS（文件与图片）
- **前端**：Vue（`frontend/`），另有 `mcp-server/`、`miniprogram/`、Chrome 扩展等接入端

## 核心抽象

| 抽象 | 定义 |
|---|---|
| `Knowledge` | 一份入库文档（文件/URL/段落/手册），带 `ParseStatus` 状态机：pending → processing → completed/failed/cancelled/deleting |
| `Chunk` | 分块单元，`ChunkType` 区分 Text / ParentText / Summary / 图片 OCR / Caption / Question 等；prev/next 链表 + ParentChunkID 父子关系 |
| `DocReader` | 解析器接口（`resolveDocReader` 按 engine/文件类型选择），Go 侧经 gRPC 调 Python 服务 |
| `chunker.SplitterConfig` | 分块配置（来自 KB 的 ChunkingConfig，支持 Parent-Child 分块） |
| `RetrieveEngine` | 检索引擎抽象，`BatchIndex` 同时写向量与关键词索引 |
| Asynq Task | 处理阶段全部异步化：`document:process` → post_process → summary/question/graph/multimodal 子任务，Stage 计数器判定最终 completed |

## 最重要的文件

- [internal/handler/knowledge.go](internal/handler/knowledge.go) — HTTP 入口（上传/URL/管理）
- [internal/application/service/knowledge_create.go](internal/application/service/knowledge_create.go) — 创建记录 + 入队
- [internal/application/service/knowledge_process.go](internal/application/service/knowledge_process.go) — 核心 worker：`ProcessDocument`(:2683)、`processChunks`(:259)、`convert`(:3097)
- [internal/application/service/knowledge_post_process.go](internal/application/service/knowledge_post_process.go) — 后处理编排（摘要/问题/图谱扇出与完成判定）
- [internal/infrastructure/chunker/](internal/infrastructure/chunker/) — Go 分块器（启发式切分、标题层级、父子分块）
- [docreader/main.py](docreader/main.py) — Python 解析服务 gRPC 入口
- [docreader/parser/](docreader/parser/) — 各格式解析器（pdf/docx/excel/pptx/markdown/web/epub/mhtml…）

## 关键设计点

1. **全异步 + 幂等**：每个阶段入口都重查 ParseStatus，取消/删除随时短路；重跑先清旧 chunks/索引/图谱再写入。
2. **先可检索，后增强**：`finalizeIndexedKnowledgeState` 让文本索引完成后立即可检索，摘要/问题/图谱/多模态在后台补充。
3. **统一中间格式**：所有格式先转 Markdown，分块在 Go 端做（不在 Python 端），保证分块策略与 KB 配置一致。
4. **父子分块**：child chunk 进向量索引用于召回，parent chunk 只入库用于扩展上下文。
5. **多模态**：图片单独落存储，OCR/Caption 各生成派生 chunk 参与检索。

## 扩展方式

- 新文档格式：在 `docreader/parser/` 加 parser 并注册到 `registry.py`
- 新检索后端：实现 `RetrieveEngine` 接口
- 新后处理阶段：在 post_process 扇出中注册 Stage + asynq task 类型

## 架构边界

- 解析（Python）与分块/索引（Go）职责分离，不要在 docreader 里做分块
- ParseStatus 状态机变更必须带 abort 复查（cancelled/deleting 竞态）
- 索引写入前必须先按 knowledge_id 删除旧数据（幂等约束）
